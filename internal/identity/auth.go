package identity

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func (s *Service) Authenticate(r *http.Request) (domain.User, bool) {
	user, authenticated, _ := s.AuthenticateWithKind(r)
	return user, authenticated
}

func (s *Service) AuthenticateWithKind(
	r *http.Request,
) (domain.User, bool, string) {
	if username, password, ok := r.BasicAuth(); ok {
		source := authenticationSource(r, s.Config.AuthTrustedProxies)
		limiter := s.authFailures
		if !limiter.beginPasswordCheck(r.Context(), source) {
			s.incBlocked()
			return domain.User{}, false, "password"
		}
		succeeded := false
		defer func() {
			limiter.finishPasswordCheck(source, succeeded)
			if !succeeded {
				s.incFailures()
			}
		}()
		if user, ok := s.meta().AuthenticatePassword(r.Context(), username, password); ok {
			succeeded = true
			recordAuthenticatedSubject(r, user, true)
			return user, true, "password"
		}
		// No local user matched: an internal OIDC provider may accept these
		// credentials via the password grant, which is how docker login and CI
		// service accounts reach an IdP that has no browser flow.
		if user, ok := s.authenticatePasswordGrant(r, username, password); ok {
			succeeded = true
			recordAuthenticatedSubject(r, user, true)
			return user, true, "oidc-password"
		}
		return domain.User{}, false, "password"
	}

	header := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		if !s.allowAuthentication(r) {
			return domain.User{}, false, "bearer"
		}
		token := strings.TrimSpace(header[len("bearer "):])
		if strings.HasPrefix(token, ociAccessTokenPrefix) {
			return s.authenticateOCIAccessToken(r, token)
		}
		if user, ok := s.meta().AuthenticateToken(r.Context(), token); ok {
			s.recordAuthentication(r, true)
			recordAuthenticatedSubject(r, user, true)
			return user, true, "api-token"
		}
		user, authenticated := s.authenticateOIDC(r, token)
		s.recordAuthentication(r, authenticated)
		recordAuthenticatedSubject(r, user, authenticated)
		return user, authenticated, "oidc-bearer"
	}
	// Cargo sends its registry token as a bare `Authorization: <token>` with no
	// scheme (no space); accept a scheme-less header as an API token so
	// `cargo publish` and `cargo yank` authenticate. It must still match a
	// stored token, so this widens ergonomics, not trust; a non-matching value
	// falls through to the ordinarily privilege-free anonymous identity.
	if header != "" && !strings.ContainsAny(header, " \t") {
		if !s.allowAuthentication(r) {
			return domain.User{}, false, "api-token"
		}
		if user, ok := s.meta().AuthenticateToken(r.Context(), header); ok {
			s.recordAuthentication(r, true)
			recordAuthenticatedSubject(r, user, true)
			return user, true, "api-token"
		}
		s.recordAuthentication(r, false)
		return domain.User{}, false, "api-token"
	}
	if header == "" {
		if cookie, err := r.Cookie(oidcSessionCookieName); err == nil {
			if !s.allowAuthentication(r) {
				return domain.User{}, false, "oidc-session"
			}
			user, authenticated := s.authenticateOIDC(r, cookie.Value)
			s.recordAuthentication(r, authenticated)
			recordAuthenticatedSubject(r, user, authenticated)
			return user, authenticated, "oidc-session"
		}
	}

	return domain.User{}, false, "anonymous"
}

func (s *Service) allowAuthentication(r *http.Request) bool {
	if s.authFailures.allow(authenticationSource(r, s.Config.AuthTrustedProxies)) {
		return true
	}
	s.incBlocked()
	return false
}

func (s *Service) recordAuthentication(r *http.Request, succeeded bool) {
	s.authFailures.record(authenticationSource(r, s.Config.AuthTrustedProxies), succeeded)
	if !succeeded {
		s.incFailures()
	}
}

func recordAuthenticatedSubject(r *http.Request, user domain.User, authenticated bool) {
	if !authenticated {
		return
	}
	httpx.SetAuthenticatedSubject(r, user.Username)
}

func (s *Service) RequirePrivilege(
	w http.ResponseWriter,
	r *http.Request,
	required string,
) bool {
	return s.requirePrivilege(w, r, required, RequestAuthentication)
}

// RequireSessionPrivilege guards the browser/API control-plane routes. On an
// unauthenticated request it answers 401 without a WWW-Authenticate: Basic
// header, so the browser never raises its native Basic-auth popup; the SPA
// reads the 401 and redirects to OIDC instead. Registry routes keep the Basic
// challenge because Docker and OCI clients require it.
func (s *Service) RequireSessionPrivilege(
	w http.ResponseWriter,
	r *http.Request,
	required string,
) bool {
	return s.requirePrivilege(w, r, required, RequestSessionAuthentication)
}

func (s *Service) requirePrivilege(
	w http.ResponseWriter,
	r *http.Request,
	required string,
	challenge func(http.ResponseWriter),
) bool {
	user, authenticated := s.Authenticate(r)
	allowed, err := s.UserHasPrivilege(r, user, authenticated, required)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"authorization_error",
			"authorization failed",
			err,
		)
		return false
	}
	if allowed {
		return true
	}
	if !authenticated {
		challenge(w)
		return false
	}
	httpx.WriteProblem(
		w,
		http.StatusForbidden,
		"forbidden",
		fmt.Sprintf("missing privilege %s", required),
	)
	return false
}

func (s *Service) UserHasPrivilege(
	r *http.Request,
	user domain.User,
	authenticated bool,
	required string,
) (bool, error) {
	if authenticated && user.External && user.Admin {
		return tokenAllows(user.TokenScopes, required), nil
	}
	var privileges []string
	var err error
	if authenticated && !user.External && user.Username != "" {
		current, resolved, err := s.meta().LocalAuthorization(r.Context(), user.Username, user.Identity)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if current.Admin {
			return tokenAllows(user.TokenScopes, required), nil
		}
		privileges = resolved
	} else if user.External {
		privileges, err = s.meta().PrivilegesForRoles(r.Context(), user.Roles)
	} else {
		privileges, err = s.meta().EffectivePrivileges(r.Context(), "")
	}
	if err != nil {
		return false, err
	}
	if !anyPrivilegeAllows(privileges, required) {
		return false, nil
	}
	return tokenAllows(user.TokenScopes, required), nil
}

func (s *Service) RequireRepositoryPrivilege(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	action string,
) bool {
	required := fmt.Sprintf("repository:%s:%s", repositoryName, action)
	return s.RequirePrivilege(w, r, required)
}

func anyPrivilegeAllows(privileges []string, required string) bool {
	for _, privilege := range privileges {
		if PrivilegeAllows(privilege, required) {
			return true
		}
	}
	return false
}

func PrivilegeAllows(granted, required string) bool {
	if granted == "*" || granted == required {
		return true
	}
	grantedParts := strings.Split(granted, ":")
	requiredParts := strings.Split(required, ":")
	if len(grantedParts) > len(requiredParts) {
		return false
	}
	for index, grantedPart := range grantedParts {
		if grantedPart == "*" {
			if index == len(grantedParts)-1 {
				return true
			}
			continue
		}
		if grantedPart != requiredParts[index] {
			return false
		}
	}
	return len(grantedParts) == len(requiredParts)
}

func tokenAllows(scopes []string, required string) bool {
	if len(scopes) == 0 {
		return true
	}
	return anyPrivilegeAllows(scopes, required)
}

func RequestAuthentication(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="suxen"`)
	httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", "authentication required")
}

// RequestSessionAuthentication answers 401 without a WWW-Authenticate challenge,
// so browsers do not raise the native Basic-auth popup on control-plane XHRs.
func RequestSessionAuthentication(w http.ResponseWriter) {
	httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", "authentication required")
}
