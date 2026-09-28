package identity

import (
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/suxen-project/suxen/internal/domain"
)

// authenticatePasswordGrant attempts the OAuth2 Resource Owner Password
// Credentials grant against every provider that opted in. It lets
// non-interactive clients (docker login, CI service accounts) authenticate
// against an internal IdP that cannot perform a browser redirect. The password
// leaves this process only towards a provider explicitly flagged
// AllowPasswordGrant, and providers are tried in order until one accepts.
func (s *Service) authenticatePasswordGrant(
	r *http.Request,
	username, password string,
) (domain.User, bool) {
	providers, err := s.meta().OIDCProviders(r.Context())
	if err != nil {
		s.requestLogger(r).Warn("list OIDC providers for password grant", "error", err)
		return domain.User{}, false
	}
	for _, provider := range providers {
		if !provider.AllowPasswordGrant {
			continue
		}
		if user, ok := s.passwordGrantOnce(r, provider, username, password); ok {
			return user, true
		}
	}
	return domain.User{}, false
}

func (s *Service) passwordGrantOnce(
	r *http.Request,
	provider domain.OIDCProvider,
	username, password string,
) (domain.User, bool) {
	discovered, _, err := s.oidcRuntime(r, provider)
	if err != nil {
		s.requestLogger(r).Warn(
			"initialize OIDC provider for password grant",
			"provider", provider.Name,
			"error", err,
		)
		return domain.User{}, false
	}
	config := oauth2.Config{
		ClientID:     provider.ClientID,
		ClientSecret: provider.ClientSecret,
		Endpoint:     discovered.Endpoint(),
		Scopes:       passwordGrantScopes(provider.Scopes),
	}
	ctx := oidc.ClientContext(r.Context(), s.client())
	token, err := config.PasswordCredentialsToken(ctx, username, password)
	if err != nil {
		// Wrong credentials, or an IdP that rejects the grant, land here; move on
		// to the next provider without revealing which one this credential targets.
		return domain.User{}, false
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		s.requestLogger(r).Warn("password grant returned no id_token", "provider", provider.Name)
		return domain.User{}, false
	}
	// Verify the returned token through the standard bearer path so signature,
	// issuer, audience, and group->role mapping stay identical to a browser login.
	return s.authenticateOIDC(r, rawIDToken)
}

// passwordGrantScopes guarantees the openid scope so the IdP returns an
// id_token, which is what carries the identity and group claims we verify.
func passwordGrantScopes(scopes []string) []string {
	result := append([]string{}, scopes...)
	for _, scope := range result {
		if scope == "openid" {
			return result
		}
	}
	return append([]string{"openid"}, result...)
}
