// Package outbound provides HTTP clients for requests to operator-configured services.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout      = 30 * time.Second
	maximumRedirects    = 10
	defaultDialTimeout  = 10 * time.Second
	defaultKeepAlive    = 30 * time.Second
	defaultIdleTimeout  = 90 * time.Second
	defaultTLSHandshake = 10 * time.Second
)

// forbiddenPrefixes contains non-public and special-use networks that must not be
// reachable through operator-supplied URLs. The list is intentionally conservative:
// internal services can still be enabled through the explicit host or CIDR allowlists.
var forbiddenPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// Resolver resolves a host into all IP addresses advertised for it.
//
// Implementations must return every address that may be selected for a connection.
// Policy rejects a host when any returned address is forbidden, which prevents a DNS
// answer containing both a public address and a sensitive internal address from being
// used to bypass the egress policy.
type Resolver interface {
	// LookupNetIP returns every candidate address for host without opening a
	// connection. Policy rejects the complete answer if any candidate is denied.
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configures an outbound network policy.
type Options struct {
	// AllowedCIDRs permits otherwise-denied addresses within these exact networks.
	AllowedCIDRs []netip.Prefix
	// AllowedHosts permits exact hosts even when they resolve to denied addresses.
	AllowedHosts []string
	// Resolver replaces the system resolver, primarily for deterministic tests.
	Resolver Resolver
}

// Policy validates and dials outbound HTTP destinations.
type Policy struct {
	allowedCIDRs []netip.Prefix
	allowedHosts map[string]struct{}
	resolver     Resolver
	dial         func(context.Context, string, string) (net.Conn, error)
}

// NewPolicy constructs an outbound policy. Hosts are exact, case-insensitive names;
// wildcards and suffix matching are intentionally unsupported.
func NewPolicy(options Options) *Policy {
	resolver := options.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	dialer := &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: defaultKeepAlive,
	}
	policy := &Policy{
		allowedCIDRs: append([]netip.Prefix(nil), options.AllowedCIDRs...),
		allowedHosts: make(map[string]struct{}, len(options.AllowedHosts)),
		resolver:     resolver,
		dial:         dialer.DialContext,
	}
	for _, host := range options.AllowedHosts {
		policy.allowedHosts[canonicalHost(host)] = struct{}{}
	}
	return policy
}

// Client returns an HTTP client that applies the policy to initial connections and
// every redirect. A non-positive timeout selects the bounded 30-second default.
func (p *Policy) Client(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return p.newClient(timeout, 0)
}

// StreamingClient bounds connection setup and the wait for response headers,
// while allowing a request or response body to stream until its caller cancels.
// A non-positive header timeout selects the 30-second default.
func (p *Policy) StreamingClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = defaultTimeout
	}
	return p.newClient(0, headerTimeout)
}

func (p *Policy) newClient(timeout, headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       defaultIdleTimeout,
			TLSHandshakeTimeout:   defaultTLSHandshake,
			ResponseHeaderTimeout: headerTimeout,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: p.checkRedirect,
	}
}

// ValidateURL resolves and checks an HTTP URL without opening a connection.
func (p *Policy) ValidateURL(ctx context.Context, target *url.URL) error {
	if target == nil {
		return errors.New("outbound URL is missing")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("outbound URL must use http or https, got %q", target.Scheme)
	}
	if target.User != nil {
		return errors.New("outbound URL must not contain user information")
	}
	host := target.Hostname()
	if host == "" {
		return errors.New("outbound URL host is missing")
	}
	_, err := p.permittedAddresses(ctx, host)
	return err
}

func (p *Policy) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= maximumRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if err := p.ValidateURL(request.Context(), request.URL); err != nil {
		return fmt.Errorf("redirect destination rejected: %w", err)
	}
	for _, previous := range via {
		if previous.Method != http.MethodGet && previous.Method != http.MethodHead {
			return errors.New("redirects are not permitted for requests with unsafe methods")
		}
	}

	// net/http rebuilds each redirect's headers from the original request.
	// Compare with that credential origin on every hop: after a cross-origin
	// redirect, a second hop on the new origin must not regain those headers.
	if !sameOrigin(via[0].URL, request.URL) {
		for _, header := range []string{
			"Authorization",
			"Cookie",
			"Proxy-Authorization",
			"Referer",
			"X-Suxen-Signature-256",
		} {
			request.Header.Del(header)
		}
	}
	return nil
}

func (p *Policy) dialContext(
	ctx context.Context,
	network string,
	address string,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse outbound address %q: %w", address, err)
	}
	addresses, err := p.permittedAddresses(ctx, host)
	if err != nil {
		return nil, err
	}

	var dialErrors []error
	for _, ip := range addresses {
		connection, dialErr := p.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, dialErr)
	}
	return nil, fmt.Errorf("dial outbound host %q: %w", host, errors.Join(dialErrors...))
}

func (p *Policy) permittedAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	canonical := canonicalHost(host)
	if canonical == "" || strings.Contains(canonical, "%") {
		return nil, fmt.Errorf("outbound host %q is invalid", host)
	}

	addresses, err := resolveHost(ctx, p.resolver, canonical)
	if err != nil {
		return nil, fmt.Errorf("resolve outbound host %q: %w", host, err)
	}
	_, hostAllowed := p.allowedHosts[canonical]
	for _, ip := range addresses {
		address := ip.Unmap()
		if !address.IsValid() {
			return nil, fmt.Errorf("resolver returned an invalid address for %q", host)
		}
		if hostAllowed || p.addressAllowed(address) {
			continue
		}
		if forbiddenAddress(address) {
			return nil, fmt.Errorf("outbound address %s for host %q is not permitted", address, host)
		}
	}
	return addresses, nil
}

func resolveHost(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("host resolved without addresses")
	}
	return addresses, nil
}

func (p *Policy) addressAllowed(address netip.Addr) bool {
	for _, prefix := range p.allowedCIDRs {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func forbiddenAddress(address netip.Addr) bool {
	for _, prefix := range forbiddenPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func sameOrigin(first, second *url.URL) bool {
	if first == nil || second == nil {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		canonicalHost(first.Hostname()) == canonicalHost(second.Hostname()) &&
		effectivePort(first) == effectivePort(second)
}

func effectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(target.Scheme, "http") {
		return "80"
	}
	return ""
}

func canonicalHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}
