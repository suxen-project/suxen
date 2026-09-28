package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
)

func TestAuthenticationFailureLimiterLocksAndRecovers(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	limiter := newAuthenticationFailureLimiter(3, time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }

	for attempt := 1; attempt <= 3; attempt++ {
		if !limiter.allow("192.0.2.10") {
			t.Fatalf("attempt %d was blocked before reaching the limit", attempt)
		}
		limiter.record("192.0.2.10", false)
	}
	if limiter.allow("192.0.2.10") {
		t.Fatal("source remained allowed after reaching the failure limit")
	}
	if !limiter.allow("192.0.2.11") {
		t.Fatal("one source blocked an unrelated source")
	}

	now = now.Add(5 * time.Minute)
	if !limiter.allow("192.0.2.10") {
		t.Fatal("source remained blocked after the lockout expired")
	}
	limiter.record("192.0.2.10", false)
	limiter.record("192.0.2.10", true)
	if entry := limiter.entries["192.0.2.10"]; entry.failures != 1 {
		t.Fatalf("successful authentication cleared failure history: %+v", entry)
	}
}

func TestAuthenticationSuccessDoesNotResetPeerFailureBudget(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(3, time.Minute, 5*time.Minute)
	const source = "192.0.2.10"
	for attempt := 1; attempt <= 2; attempt++ {
		if !limiter.allow(source) {
			t.Fatalf("attempt %d was blocked before reaching the limit", attempt)
		}
		limiter.record(source, false)
	}
	limiter.record(source, true)
	if !limiter.allow(source) {
		t.Fatal("successful authentication caused an early lockout")
	}
	limiter.record(source, false)
	if limiter.allow(source) {
		t.Fatal("successful authentication reset the peer's failure budget")
	}
}

func TestOIDCCookieSecurityUsesPublicOriginOrTrustedProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://registry.example/auth/oidc/idp/login", nil)
	request.RemoteAddr = "192.0.2.15:43120"
	request.Header.Set("X-Forwarded-Proto", "https")

	service := &Service{Config: config.Config{PublicURL: "https://registry.example"}}
	if !service.oidcCookieSecure(request) {
		t.Fatal("HTTPS public origin produced a non-Secure OIDC cookie")
	}
	service.Config.PublicURL = "http://registry.example"
	if service.oidcCookieSecure(request) {
		t.Fatal("HTTP public origin was overridden by an untrusted forwarding header")
	}

	service.Config.PublicURL = ""
	service.Config.AuthTrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.20.0.0/24")}
	if service.oidcCookieSecure(request) {
		t.Fatal("untrusted peer supplied cookie scheme forwarding")
	}
	request.RemoteAddr = "10.20.0.8:43120"
	if !service.oidcCookieSecure(request) {
		t.Fatal("trusted TLS-terminating proxy did not produce a Secure OIDC cookie")
	}
}

func TestAuthenticationFailureWindowExpires(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	limiter := newAuthenticationFailureLimiter(2, time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }

	limiter.record("192.0.2.10", false)
	now = now.Add(time.Minute)
	limiter.record("192.0.2.10", false)
	if !limiter.allow("192.0.2.10") {
		t.Fatal("failure outside the window triggered a lockout")
	}
}

func TestLateFailureDoesNotClearActiveLockout(t *testing.T) {
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	limiter := newAuthenticationFailureLimiter(2, time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }
	const source = "192.0.2.10"

	limiter.record(source, false)
	now = now.Add(9 * time.Second)
	if !limiter.allow(source) {
		t.Fatal("slow credential check could not start before lockout")
	}
	now = now.Add(time.Second)
	limiter.record(source, false)
	blockedUntil := limiter.entries[source].blockedUntil
	if limiter.allow(source) {
		t.Fatal("second failure did not trigger lockout")
	}

	// The admitted check finishes after the failure window but before lockout
	// expires. Its late failure must not start a new, unblocked window.
	now = now.Add(51 * time.Second)
	limiter.record(source, false)
	if limiter.allow(source) {
		t.Fatal("late failure cleared an active lockout")
	}
	if got := limiter.entries[source].blockedUntil; !got.Equal(blockedUntil) {
		t.Fatalf("late failure changed lockout end to %v, want %v", got, blockedUntil)
	}
	now = blockedUntil
	if !limiter.allow(source) {
		t.Fatal("source remained blocked after the original lockout expired")
	}
}

func TestAuthenticationFailureLimiterBoundsSourceState(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(3, time.Minute, 5*time.Minute)
	for index := 0; index <= maximumAuthenticationSources; index++ {
		source := netip.AddrFrom4([4]byte{192, 0, byte(index / 256), byte(index % 256)}).String()
		limiter.record(source, false)
	}
	if entries := len(limiter.entries); entries != maximumAuthenticationSources {
		t.Fatalf("tracked sources = %d, want %d", entries, maximumAuthenticationSources)
	}
}

func TestPasswordAdmissionReservesRemainingFailureBudget(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(3, time.Minute, 5*time.Minute)
	const source = "192.0.2.10"
	limiter.record(source, false)
	limiter.record(source, false)
	if !limiter.beginPasswordCheck(context.Background(), source) {
		t.Fatal("remaining failure budget was unavailable")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if limiter.beginPasswordCheck(canceled, source) {
		t.Fatal("concurrent request exceeded the remaining failure budget")
	}
	limiter.finishPasswordCheck(source, true)
	if !limiter.beginPasswordCheck(context.Background(), source) {
		t.Fatal("successful request did not release its admission")
	}
	limiter.finishPasswordCheck(source, false)
	if limiter.beginPasswordCheck(context.Background(), source) {
		t.Fatal("completed failure did not trigger lockout")
	}
	if limiter.activePasswordChecks != 0 || len(limiter.passwordChecks) != 0 {
		t.Fatalf("password admissions leaked: active=%d sources=%d", limiter.activePasswordChecks, len(limiter.passwordChecks))
	}
}

func TestPasswordAdmissionWaitsForCapacityAndHonorsCancellation(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(10, time.Minute, 5*time.Minute)
	const source = "192.0.2.10"
	for range maximumConcurrentPasswordChecksPerPeer {
		if !limiter.beginPasswordCheck(context.Background(), source) {
			t.Fatal("initial password check was denied")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan bool, 1)
	go func() { queued <- limiter.beginPasswordCheck(ctx, source) }()
	waitForPasswordWaiters(t, limiter, 1)
	select {
	case <-queued:
		t.Fatal("queued check completed before capacity was released")
	default:
	}
	cancel()
	select {
	case admitted := <-queued:
		if admitted {
			t.Fatal("canceled request was admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request remained queued")
	}
	waitForPasswordWaiters(t, limiter, 0)

	queued = make(chan bool, 1)
	go func() { queued <- limiter.beginPasswordCheck(context.Background(), source) }()
	waitForPasswordWaiters(t, limiter, 1)
	limiter.finishPasswordCheck(source, true)
	select {
	case admitted := <-queued:
		if !admitted {
			t.Fatal("queued request was rejected after capacity became available")
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not start after capacity became available")
	}
	limiter.finishPasswordCheck(source, true)
	limiter.finishPasswordCheck(source, true)
	waitForPasswordWaiters(t, limiter, 0)
}

func TestPasswordAdmissionBoundsWaitingRequests(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(100, time.Minute, 5*time.Minute)
	const source = "192.0.2.10"
	for range maximumConcurrentPasswordChecksPerPeer {
		if !limiter.beginPasswordCheck(context.Background(), source) {
			t.Fatal("initial password check was denied")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan bool, maximumWaitingPasswordChecksPerPeer)
	for range maximumWaitingPasswordChecksPerPeer {
		go func() { results <- limiter.beginPasswordCheck(ctx, source) }()
	}
	waitForPasswordWaiters(t, limiter, maximumWaitingPasswordChecksPerPeer)
	if limiter.beginPasswordCheck(context.Background(), source) {
		t.Fatal("request exceeded the bounded per-peer queue")
	}
	cancel()
	for range maximumWaitingPasswordChecksPerPeer {
		select {
		case admitted := <-results:
			if admitted {
				t.Fatal("canceled waiter was admitted")
			}
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not leave the queue")
		}
	}
	waitForPasswordWaiters(t, limiter, 0)
	for range maximumConcurrentPasswordChecksPerPeer {
		limiter.finishPasswordCheck(source, true)
	}
}

func TestQueuedPasswordCheckObservesCompletedFailureLockout(t *testing.T) {
	limiter := newAuthenticationFailureLimiter(1, time.Minute, 5*time.Minute)
	const source = "192.0.2.10"
	if !limiter.beginPasswordCheck(context.Background(), source) {
		t.Fatal("first check was denied")
	}
	queued := make(chan bool, 1)
	go func() { queued <- limiter.beginPasswordCheck(context.Background(), source) }()
	waitForPasswordWaiters(t, limiter, 1)
	limiter.finishPasswordCheck(source, false)
	select {
	case admitted := <-queued:
		if admitted {
			t.Fatal("queued check bypassed the completed failure lockout")
		}
	case <-time.After(time.Second):
		t.Fatal("queued check did not observe the lockout")
	}
	waitForPasswordWaiters(t, limiter, 0)
}

func waitForPasswordWaiters(t *testing.T, limiter *authenticationFailureLimiter, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		limiter.mu.Lock()
		got := limiter.activeWaiters
		limiter.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("password waiters did not reach %d", want)
}

func TestAuthenticationSourceUsesSocketPeerOnly(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://registry.example/api/v1/whoami", nil)
	request.RemoteAddr = "192.0.2.15:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.25")

	if source := authenticationSource(request, nil); source != "192.0.2.15" {
		t.Fatalf("authentication source = %q, want socket peer", source)
	}
}

func TestAuthenticationSourceUsesForwardingFromTrustedPeer(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://registry.example/api/v1/whoami", nil)
	request.RemoteAddr = "10.20.0.8:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.25, 10.20.0.7")
	trusted := []netip.Prefix{netip.MustParsePrefix("10.20.0.0/24")}

	if source := authenticationSource(request, trusted); source != "198.51.100.25" {
		t.Fatalf("authentication source = %q, want forwarded client", source)
	}
}
