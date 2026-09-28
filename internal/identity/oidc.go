package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/suxen-project/suxen/internal/domain"
)

type cachedOIDCVerifier struct {
	fingerprint string
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
}

const (
	maximumConcurrentOIDCDiscoveries       = 4
	maximumOIDCDiscoveryWaitersPerProvider = 8
	defaultOIDCDiscoveryTimeout            = 30 * time.Second
)

var errOIDCDiscoveryBusy = errors.New("OIDC discovery capacity exhausted")

type oidcDiscoveryKey struct {
	name        string
	fingerprint string
	generation  uint64
}

type oidcDiscoveryFlight struct {
	key      oidcDiscoveryKey
	done     chan struct{}
	waiters  int
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	err      error
}

type oidcStandardClaims struct {
	Subject           string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

func (s *Service) authenticateOIDC(r *http.Request, rawToken string) (domain.User, bool) {
	issuer, err := unverifiedTokenIssuer(rawToken)
	if err != nil {
		return domain.User{}, false
	}
	provider, err := s.meta().OIDCProviderByIssuer(r.Context(), issuer)
	if err != nil {
		return domain.User{}, false
	}
	_, verifier, err := s.oidcRuntime(r, provider)
	if err != nil {
		if !errors.Is(err, errOIDCDiscoveryBusy) {
			s.requestLogger(r).Warn("initialize OIDC verifier", "provider", provider.Name, "error", err)
		}
		return domain.User{}, false
	}
	idToken, err := verifier.Verify(r.Context(), rawToken)
	if err != nil {
		return domain.User{}, false
	}

	var standard oidcStandardClaims
	if err := idToken.Claims(&standard); err != nil {
		return domain.User{}, false
	}
	groups, err := oidcGroups(idToken, provider.GroupsClaim)
	if err != nil {
		return domain.User{}, false
	}
	roles := append([]string{}, provider.DefaultRoles...)
	for _, group := range groups {
		roles = append(roles, provider.GroupRoles[group]...)
	}

	username := standard.PreferredUsername
	if username == "" {
		username = standard.Email
	}
	if username == "" {
		username = standard.Subject
	}
	return domain.User{
		Username: username,
		External: true,
		Roles:    uniqueSortedStrings(roles),
	}, true
}

func (s *Service) oidcVerifier(
	r *http.Request,
	provider domain.OIDCProvider,
) (*oidc.IDTokenVerifier, error) {
	_, verifier, err := s.oidcRuntime(r, provider)
	return verifier, err
}

func (s *Service) oidcRuntime(
	r *http.Request,
	provider domain.OIDCProvider,
) (*oidc.Provider, *oidc.IDTokenVerifier, error) {
	if err := r.Context().Err(); err != nil {
		return nil, nil, err
	}
	fingerprint := provider.Issuer + "\x00" + provider.ClientID
	s.oidcMu.Lock()
	cached, found := s.oidcVerifiers[provider.Name]
	if found && cached.fingerprint == fingerprint {
		s.oidcMu.Unlock()
		return cached.provider, cached.verifier, nil
	}
	generation := s.oidcGenerations[provider.Name]
	key := oidcDiscoveryKey{provider.Name, fingerprint, generation}
	s.oidcLatestFingerprint[provider.Name] = fingerprint
	flight := s.oidcDiscoveries[key]
	if flight != nil {
		if flight.waiters >= maximumOIDCDiscoveryWaitersPerProvider {
			s.oidcMu.Unlock()
			return nil, nil, errOIDCDiscoveryBusy
		}
		flight.waiters++
	} else {
		if s.activeOIDCDiscoveries >= maximumConcurrentOIDCDiscoveries {
			s.oidcMu.Unlock()
			return nil, nil, errOIDCDiscoveryBusy
		}
		flight = &oidcDiscoveryFlight{
			key:     key,
			done:    make(chan struct{}),
			waiters: 1,
		}
		s.oidcDiscoveries[key] = flight
		s.activeOIDCDiscoveries++
		go s.discoverOIDCProvider(r.Context(), provider, flight)
	}
	s.oidcMu.Unlock()
	defer func() {
		s.oidcMu.Lock()
		flight.waiters--
		s.oidcMu.Unlock()
	}()

	select {
	case <-r.Context().Done():
		return nil, nil, r.Context().Err()
	case <-flight.done:
		return flight.provider, flight.verifier, flight.err
	}
}

func (s *Service) discoverOIDCProvider(
	requestContext context.Context,
	provider domain.OIDCProvider,
	flight *oidcDiscoveryFlight,
) {
	timeout := s.Config.OutboundTimeout
	if timeout <= 0 {
		timeout = defaultOIDCDiscoveryTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), timeout)
	defer cancel()
	ctx = oidc.ClientContext(ctx, s.client())
	discovered, err := oidc.NewProvider(ctx, provider.Issuer)
	var verifier *oidc.IDTokenVerifier
	if err == nil {
		verifier = discovered.Verifier(&oidc.Config{ClientID: provider.ClientID})
	}

	s.oidcMu.Lock()
	if err == nil && flight.key.generation == s.oidcGenerations[provider.Name] &&
		flight.key.fingerprint == s.oidcLatestFingerprint[provider.Name] {
		s.oidcVerifiers[provider.Name] = cachedOIDCVerifier{
			fingerprint: flight.key.fingerprint,
			provider:    discovered,
			verifier:    verifier,
		}
	}
	flight.provider, flight.verifier, flight.err = discovered, verifier, err
	delete(s.oidcDiscoveries, flight.key)
	s.activeOIDCDiscoveries--
	close(flight.done)
	s.oidcMu.Unlock()
}

func (s *Service) InvalidateOIDCVerifier(name string) {
	s.oidcMu.Lock()
	delete(s.oidcVerifiers, name)
	s.oidcGenerations[name]++
	s.oidcMu.Unlock()
}

func unverifiedTokenIssuer(rawToken string) (string, error) {
	issuer, _, err := unverifiedTokenClaims(rawToken)
	return issuer, err
}

// unverifiedTokenClaims decodes the issuer and expiry from a JWT without
// checking its signature. Callers must already trust the token (it was verified
// on this request); the decoded values are only a hint, never an authorization.
func unverifiedTokenClaims(rawToken string) (issuer string, expiresAt int64, err error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return "", 0, fmt.Errorf("JWT must have three segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
		Expiry int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", 0, fmt.Errorf("decode JWT claims: %w", err)
	}
	if claims.Issuer == "" {
		return "", 0, fmt.Errorf("JWT issuer is missing")
	}
	return claims.Issuer, claims.Expiry, nil
}

// OIDCSessionHint returns the provider name and expiry (unix seconds) of the
// browser OIDC session on this request, so the UI can schedule a silent re-auth
// before it lapses. It reads the already-authenticated session cookie and
// decodes the expiry without a second signature check; the caller uses it only
// as a refresh hint. ok is false when there is no OIDC session cookie.
func (s *Service) OIDCSessionHint(r *http.Request) (provider string, expiresAt int64, ok bool) {
	cookie, err := r.Cookie(oidcSessionCookieName)
	if err != nil {
		return "", 0, false
	}
	issuer, expiry, err := unverifiedTokenClaims(cookie.Value)
	if err != nil {
		return "", 0, false
	}
	found, err := s.meta().OIDCProviderByIssuer(r.Context(), issuer)
	if err != nil {
		return "", 0, false
	}
	return found.Name, expiry, true
}

func oidcGroups(idToken *oidc.IDToken, claimName string) ([]string, error) {
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	raw, found := claims[claimName]
	if !found {
		return []string{}, nil
	}
	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}
	var group string
	if err := json.Unmarshal(raw, &group); err != nil {
		return nil, fmt.Errorf("OIDC group claim %q must be a string or string array", claimName)
	}
	if group == "" {
		return []string{}, nil
	}
	return []string{group}, nil
}
