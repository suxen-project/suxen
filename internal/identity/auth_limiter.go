package identity

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	defaultAuthenticationFailureLimit  = 10
	defaultAuthenticationFailureWindow = time.Minute
	defaultAuthenticationLockout       = 5 * time.Minute
	maximumAuthenticationSources       = 4096
	// Each local password check can allocate 64 MiB for Argon2. Bound both
	// the process-wide allocation and a single peer's share of it.
	maximumConcurrentPasswordChecks        = 4
	maximumConcurrentPasswordChecksPerPeer = 2
	maximumWaitingPasswordChecks           = 64
	maximumWaitingPasswordChecksPerPeer    = 16
	passwordCheckWaitLimit                 = 15 * time.Second
)

type authenticationFailureLimiter struct {
	mu                    sync.Mutex
	entries               map[string]authenticationFailureEntry
	limit                 int
	window                time.Duration
	lockout               time.Duration
	now                   func() time.Time
	passwordChecks        map[string]int
	activePasswordChecks  int
	waitingPasswordChecks map[string]int
	activeWaiters         int
	changed               chan struct{}
}

type authenticationFailureEntry struct {
	failures      int
	windowStarted time.Time
	blockedUntil  time.Time
	lastSeen      time.Time
}

func newAuthenticationFailureLimiter(
	limit int,
	window time.Duration,
	lockout time.Duration,
) *authenticationFailureLimiter {
	if limit <= 0 {
		limit = defaultAuthenticationFailureLimit
	}
	if window <= 0 {
		window = defaultAuthenticationFailureWindow
	}
	if lockout <= 0 {
		lockout = defaultAuthenticationLockout
	}
	return &authenticationFailureLimiter{
		entries:               make(map[string]authenticationFailureEntry),
		passwordChecks:        make(map[string]int),
		waitingPasswordChecks: make(map[string]int),
		changed:               make(chan struct{}),
		limit:                 limit,
		window:                window,
		lockout:               lockout,
		now:                   time.Now,
	}
}

func (limiter *authenticationFailureLimiter) allow(source string) bool {
	if limiter == nil {
		return true
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	return limiter.allowLocked(source)
}

func (limiter *authenticationFailureLimiter) allowLocked(source string) bool {
	entry, found := limiter.entries[source]
	if !found {
		return true
	}
	now := limiter.now()
	if now.Before(entry.blockedUntil) {
		entry.lastSeen = now
		limiter.entries[source] = entry
		return false
	}
	if !entry.blockedUntil.IsZero() {
		delete(limiter.entries, source)
	}
	return true
}

// beginPasswordCheck bounds active verification and allows a short, bounded
// wait for ordinary concurrent downloads. Waiting never reserves failure
// budget: it is checked again under the lock immediately before admission.
func (limiter *authenticationFailureLimiter) beginPasswordCheck(ctx context.Context, source string) bool {
	if limiter == nil {
		return true
	}
	waitCtx, cancel := context.WithTimeout(ctx, passwordCheckWaitLimit)
	defer cancel()
	waiting := false
	for {
		limiter.mu.Lock()
		if waitCtx.Err() != nil || !limiter.allowLocked(source) {
			limiter.leavePasswordQueueLocked(source, waiting)
			limiter.mu.Unlock()
			return false
		}
		entry, found := limiter.entries[source]
		failureBudgetAvailable := (!found || limiter.now().Sub(entry.windowStarted) >= limiter.window ||
			entry.failures+limiter.passwordChecks[source] < limiter.limit) &&
			limiter.passwordChecks[source] < limiter.limit
		if failureBudgetAvailable &&
			limiter.activePasswordChecks < maximumConcurrentPasswordChecks &&
			limiter.passwordChecks[source] < maximumConcurrentPasswordChecksPerPeer {
			limiter.leavePasswordQueueLocked(source, waiting)
			limiter.passwordChecks[source]++
			limiter.activePasswordChecks++
			limiter.mu.Unlock()
			return true
		}
		if !waiting {
			if limiter.activeWaiters >= maximumWaitingPasswordChecks ||
				limiter.waitingPasswordChecks[source] >= maximumWaitingPasswordChecksPerPeer {
				limiter.mu.Unlock()
				return false
			}
			limiter.activeWaiters++
			limiter.waitingPasswordChecks[source]++
			waiting = true
		}
		changed := limiter.changed
		limiter.mu.Unlock()
		select {
		case <-changed:
		case <-waitCtx.Done():
		}
	}
}

func (limiter *authenticationFailureLimiter) leavePasswordQueueLocked(source string, waiting bool) {
	if !waiting {
		return
	}
	limiter.activeWaiters--
	limiter.waitingPasswordChecks[source]--
	if limiter.waitingPasswordChecks[source] == 0 {
		delete(limiter.waitingPasswordChecks, source)
	}
}

// finishPasswordCheck releases capacity and records the result under the
// same lock, so the next admission observes a completed failure immediately.
func (limiter *authenticationFailureLimiter) finishPasswordCheck(source string, succeeded bool) {
	if limiter == nil {
		return
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.passwordChecks[source]--
	if limiter.passwordChecks[source] == 0 {
		delete(limiter.passwordChecks, source)
	}
	limiter.activePasswordChecks--
	limiter.recordLocked(source, succeeded)
	close(limiter.changed)
	limiter.changed = make(chan struct{})
}

func (limiter *authenticationFailureLimiter) record(source string, succeeded bool) {
	if limiter == nil {
		return
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.recordLocked(source, succeeded)
}

func (limiter *authenticationFailureLimiter) recordLocked(source string, succeeded bool) {
	if succeeded {
		// A valid credential may belong to a different principal than the
		// credentials being guessed. Keep the peer's failures until the window
		// expires so a valid token cannot reset the guessing budget.
		return
	}
	now := limiter.now()
	entry, found := limiter.entries[source]
	if found && now.Before(entry.blockedUntil) {
		// A check admitted before lockout can finish after its failure window.
		// It must not reset (or extend) an already active lockout.
		entry.lastSeen = now
		limiter.entries[source] = entry
		return
	}
	if !found || now.Sub(entry.windowStarted) >= limiter.window {
		entry = authenticationFailureEntry{
			windowStarted: now,
		}
	}
	entry.failures++
	entry.lastSeen = now
	if entry.failures >= limiter.limit {
		entry.blockedUntil = now.Add(limiter.lockout)
	}
	if _, found := limiter.entries[source]; !found &&
		len(limiter.entries) >= maximumAuthenticationSources {
		limiter.evictOldest()
	}
	limiter.entries[source] = entry
}

func (limiter *authenticationFailureLimiter) evictOldest() {
	oldestSource := ""
	var oldest time.Time
	for source, entry := range limiter.entries {
		if oldestSource == "" || entry.lastSeen.Before(oldest) {
			oldestSource = source
			oldest = entry.lastSeen
		}
	}
	if oldestSource != "" {
		delete(limiter.entries, oldestSource)
	}
}

func authenticationSource(r *http.Request, trustedProxies []netip.Prefix) string {
	remoteAddress := strings.TrimSpace(r.RemoteAddr)
	peer := remoteAddress
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil && host != "" {
		peer = host
	}
	peerAddress, err := netip.ParseAddr(strings.Trim(peer, "[]"))
	if err == nil && addressInPrefixes(peerAddress, trustedProxies) {
		if forwarded := forwardedAuthenticationSource(
			r.Header.Values("X-Forwarded-For"),
			trustedProxies,
		); forwarded != "" {
			return forwarded
		}
	}
	if peer != "" {
		return peer
	}
	return "unknown"
}

func requestFromTrustedProxy(r *http.Request, trustedProxies []netip.Prefix) bool {
	remoteAddress := strings.TrimSpace(r.RemoteAddr)
	peer := remoteAddress
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil && host != "" {
		peer = host
	}
	peerAddress, err := netip.ParseAddr(strings.Trim(peer, "[]"))
	return err == nil && addressInPrefixes(peerAddress, trustedProxies)
}

func forwardedAuthenticationSource(
	headerValues []string,
	trustedProxies []netip.Prefix,
) string {
	addresses := make([]netip.Addr, 0)
	for _, headerValue := range headerValues {
		for _, part := range strings.Split(headerValue, ",") {
			address, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return ""
			}
			addresses = append(addresses, address)
		}
	}
	for index := len(addresses) - 1; index >= 0; index-- {
		if !addressInPrefixes(addresses[index], trustedProxies) {
			return addresses[index].String()
		}
	}
	if len(addresses) > 0 {
		return addresses[0].String()
	}
	return ""
}

func addressInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
