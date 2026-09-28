package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"golang.org/x/oauth2"
)

const (
	oidcLoginCookieName   = "suxen_oidc_login"
	oidcSessionCookieName = "suxen_oidc_token"
	oidcLoginLifetime     = 10 * time.Minute

	// LoginCookieName is the OIDC login transaction cookie.
	LoginCookieName = oidcLoginCookieName
	// SessionCookieName is the OIDC session cookie.
	SessionCookieName = oidcSessionCookieName
)

type oidcLoginTransaction struct {
	Provider     string `json:"provider"`
	State        string `json:"state"`
	Verifier     string `json:"verifier"`
	Nonce        string `json:"nonce"`
	RedirectPath string `json:"redirectPath"`
	ExpiresAt    int64  `json:"expiresAt"`
	// Silent marks a prompt=none re-authentication started by the UI before the
	// session lapses. On an interaction_required/login_required outcome the
	// callback redirects back to the app instead of showing an error, so the
	// still-valid session keeps working and the UI falls back to a visible login
	// only once it truly expires.
	Silent bool `json:"silent,omitempty"`
}

// LoginTransaction is the signed OIDC login cookie payload.
type LoginTransaction = oidcLoginTransaction

func (s *Service) HandleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	parts := httpx.SplitPath(strings.TrimPrefix(r.URL.Path, "/auth/oidc/"))
	if len(parts) != 2 {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "OIDC login route not found")
		return
	}
	providerName := parts[0]
	switch parts[1] {
	case "login":
		s.startOIDCLogin(w, r, providerName)
	case "callback":
		s.completeOIDCLogin(w, r, providerName)
	case "logout":
		s.endOIDCLogin(w, r)
	default:
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "OIDC login route not found")
	}
}

func (s *Service) requireOIDCLoginEnabled(w http.ResponseWriter) bool {
	if s.Config.OIDCStateSecret == "" {
		httpx.WriteProblem(
			w,
			http.StatusServiceUnavailable,
			"oidc_login_disabled",
			"SUXEN_OIDC_STATE_SECRET is required for interactive OIDC login",
		)
		return false
	}
	return true
}

func (s *Service) startOIDCLogin(w http.ResponseWriter, r *http.Request, providerName string) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireOIDCLoginEnabled(w) {
		return
	}
	provider, err := s.meta().OIDCProvider(r.Context(), providerName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	discovered, _, err := s.oidcRuntime(r, provider)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"oidc_discovery_failed",
			"OIDC discovery failed",
			err,
		)
		return
	}

	// A UI silent refresh asks for prompt=none: the IdP re-authenticates against
	// its existing session without showing any UI, or returns login_required.
	silent := r.URL.Query().Get("prompt") == "none"
	verifier := httpx.RandomSecret(32)
	challengeHash := sha256.Sum256([]byte(verifier))
	transaction := oidcLoginTransaction{
		Provider:     provider.Name,
		State:        httpx.RandomSecret(24),
		Verifier:     verifier,
		Nonce:        httpx.RandomSecret(24),
		RedirectPath: safeRedirectPath(r.URL.Query().Get("redirect")),
		ExpiresAt:    time.Now().Add(oidcLoginLifetime).Unix(),
		Silent:       silent,
	}
	encodedTransaction, err := s.signOIDCLoginTransaction(transaction)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"oidc_state_failed",
			"create OIDC login state failed",
			err,
		)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcLoginCookieName,
		Value:    encodedTransaction,
		Path:     "/auth/oidc/",
		MaxAge:   int(oidcLoginLifetime.Seconds()),
		HttpOnly: true,
		Secure:   s.oidcCookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})

	oauthConfig := s.oidcOAuthConfig(r, provider, discovered.Endpoint())
	authParams := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam(
			"code_challenge",
			base64.RawURLEncoding.EncodeToString(challengeHash[:]),
		),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("nonce", transaction.Nonce),
	}
	if silent {
		authParams = append(authParams, oauth2.SetAuthURLParam("prompt", "none"))
	}
	authorizationURL := oauthConfig.AuthCodeURL(transaction.State, authParams...)
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

func (s *Service) completeOIDCLogin(w http.ResponseWriter, r *http.Request, providerName string) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireOIDCLoginEnabled(w) {
		return
	}
	loginCookie, err := r.Cookie(oidcLoginCookieName)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_oidc_state", "OIDC login cookie is missing")
		return
	}
	s.clearOIDCCookie(w, r, oidcLoginCookieName, "/auth/oidc/")
	transaction, err := s.VerifyOIDCLoginTransaction(loginCookie.Value)
	if err != nil || transaction.Provider != providerName || transaction.State != r.URL.Query().Get("state") {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_oidc_state", "OIDC login state is invalid")
		return
	}
	if transaction.ExpiresAt < time.Now().Unix() {
		httpx.WriteProblem(w, http.StatusBadRequest, "expired_oidc_state", "OIDC login state has expired")
		return
	}
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		// A silent (prompt=none) attempt that cannot re-authenticate without
		// interaction returns here (login_required/interaction_required). The
		// session being refreshed is still valid, so send the user back to the
		// app rather than an error page; the UI shows a visible login only once
		// the session truly lapses.
		if transaction.Silent {
			http.Redirect(w, r, transaction.RedirectPath, http.StatusFound)
			return
		}
		httpx.WriteProblem(w, http.StatusUnauthorized, "oidc_provider_error", providerError)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "missing_oidc_code", "authorization code is missing")
		return
	}

	provider, err := s.meta().OIDCProvider(r.Context(), providerName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	discovered, verifier, err := s.oidcRuntime(r, provider)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"oidc_discovery_failed",
			"OIDC discovery failed",
			err,
		)
		return
	}
	oauthConfig := s.oidcOAuthConfig(r, provider, discovered.Endpoint())
	contextWithClient := oidc.ClientContext(r.Context(), s.client())
	token, err := oauthConfig.Exchange(
		contextWithClient,
		code,
		oauth2.VerifierOption(transaction.Verifier),
	)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"oidc_exchange_failed",
			"OIDC token exchange failed",
			err,
		)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"oidc_token_missing",
			"provider did not return an ID token",
			fmt.Errorf("OIDC provider %q did not return an ID token", provider.Name),
		)
		return
	}
	idToken, err := verifier.Verify(contextWithClient, rawIDToken)
	if err != nil {
		s.rejectInvalidOIDCToken(w, r, provider.Name, err)
		return
	}
	if idToken.Nonce != transaction.Nonce {
		httpx.WriteProblem(w, http.StatusUnauthorized, "invalid_oidc_nonce", "ID token nonce does not match")
		return
	}

	maxAge := int(time.Until(idToken.Expiry).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcSessionCookieName,
		Value:    rawIDToken,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  idToken.Expiry,
		HttpOnly: true,
		Secure:   s.oidcCookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, transaction.RedirectPath, http.StatusFound)
}

func (s *Service) rejectInvalidOIDCToken(w http.ResponseWriter, r *http.Request, provider string, err error) {
	s.requestLogger(r).Warn("verify OIDC login ID token", "provider", provider, "error", err)
	httpx.WriteProblem(w, http.StatusUnauthorized, "invalid_oidc_token", "ID token is invalid")
}

func (s *Service) endOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}
	s.clearOIDCCookie(w, r, oidcSessionCookieName, "/")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) oidcOAuthConfig(
	r *http.Request,
	provider domain.OIDCProvider,
	endpoint oauth2.Endpoint,
) oauth2.Config {
	return oauth2.Config{
		ClientID:     provider.ClientID,
		ClientSecret: provider.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  s.oidcCallbackURL(r, provider.Name),
		Scopes:       oidcScopes(provider.Scopes),
	}
}

func (s *Service) oidcCallbackURL(r *http.Request, providerName string) string {
	path := "/auth/oidc/" + url.PathEscape(providerName) + "/callback"
	if s.Config.PublicURL != "" {
		return s.Config.PublicURL + path
	}
	scheme := "http"
	if httpx.RequestUsesTLS(r) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, r.Host, path)
}

func (s *Service) signOIDCLoginTransaction(transaction oidcLoginTransaction) (string, error) {
	payload, err := json.Marshal(transaction)
	if err != nil {
		return "", fmt.Errorf("encode OIDC login state: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := hmac.New(sha256.New, []byte(s.Config.OIDCStateSecret))
	_, _ = signature.Write([]byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), nil
}

func (s *Service) VerifyOIDCLoginTransaction(value string) (oidcLoginTransaction, error) {
	var transaction oidcLoginTransaction
	if s.Config.OIDCStateSecret == "" {
		return transaction, fmt.Errorf("OIDC login state secret is not configured")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return transaction, fmt.Errorf("OIDC login state must contain two segments")
	}
	expected := hmac.New(sha256.New, []byte(s.Config.OIDCStateSecret))
	_, _ = expected.Write([]byte(parts[0]))
	actual, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(actual, expected.Sum(nil)) {
		return transaction, fmt.Errorf("OIDC login state signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return transaction, fmt.Errorf("decode OIDC login state: %w", err)
	}
	if err := json.Unmarshal(payload, &transaction); err != nil {
		return transaction, fmt.Errorf("decode OIDC login transaction: %w", err)
	}
	return transaction, nil
}

func oidcScopes(configured []string) []string {
	scopes := append([]string{}, configured...)
	for _, scope := range scopes {
		if scope == oidc.ScopeOpenID {
			return uniqueSortedStrings(scopes)
		}
	}
	return uniqueSortedStrings(append(scopes, oidc.ScopeOpenID))
}

func safeRedirectPath(candidate string) string {
	if candidate == "" {
		return "/"
	}
	if ambiguousRedirectPath(candidate) {
		return "/"
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" ||
		!strings.HasPrefix(parsed.Path, "/") || ambiguousRedirectPath(parsed.Path) {
		return "/"
	}
	if strings.HasPrefix(candidate, "//") {
		return "/"
	}
	return candidate
}

// ambiguousRedirectPath rejects browser separators before and after URL
// decoding. Go treats a backslash as a path character, while WHATWG browser
// parsing treats it as a slash for special schemes and can turn /\host into
// an external redirect.
func ambiguousRedirectPath(candidate string) bool {
	decoded := candidate
	for range 4 {
		for _, character := range decoded {
			if character == '\\' || character < 0x20 || character == 0x7f {
				return true
			}
		}
		if strings.HasPrefix(decoded, "//") {
			return true
		}
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return true
		}
		if next == decoded {
			return false
		}
		decoded = next
	}
	return true
}

func (s *Service) oidcCookieSecure(r *http.Request) bool {
	if s.Config.PublicURL != "" {
		publicURL, err := url.Parse(s.Config.PublicURL)
		return err == nil && strings.EqualFold(publicURL.Scheme, "https")
	}
	if httpx.RequestUsesTLS(r) {
		return true
	}
	if !requestFromTrustedProxy(r, s.Config.AuthTrustedProxies) {
		return false
	}
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

func (s *Service) clearOIDCCookie(w http.ResponseWriter, r *http.Request, name string, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   s.oidcCookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}
