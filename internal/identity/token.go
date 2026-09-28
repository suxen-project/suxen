package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

const (
	// AccessTokenPrefix identifies short-lived registry bearer tokens.
	AccessTokenPrefix    = "suxen.oci.v1."
	ociAccessTokenPrefix = AccessTokenPrefix
	ociAccessTokenTTL    = 5 * time.Minute
	// AccessTokenTTL is the registry bearer token lifetime.
	AccessTokenTTL = ociAccessTokenTTL
)

// AccessToken is the signed payload for a registry bearer token.
type AccessToken struct {
	Repository string `json:"repo"`
	Subject    string `json:"sub"`
	// AccountIdentity binds a local bearer to the account generation that
	// authenticated when the bearer was issued.
	AccountIdentity string   `json:"aid,omitempty"`
	ExpiresAt       int64    `json:"exp"`
	Actions         []string `json:"act"`
	Admin           bool     `json:"adm,omitempty"`
	// Local distinguishes local accounts named "anonymous" from anonymous
	// handshakes. Older ambiguous tokens are treated as anonymous below.
	Local    bool     `json:"loc,omitempty"`
	External bool     `json:"ext,omitempty"`
	Roles    []string `json:"rol,omitempty"`
}

func ociDistributionAuthPath(requestPath string) bool {
	return strings.Trim(requestPath, "/") == "token"
}

// DistributionAuthPath reports whether requestPath is the OCI token endpoint.
func DistributionAuthPath(requestPath string) bool {
	return ociDistributionAuthPath(requestPath)
}

func (s *Service) HandleOCIToken(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	secret := s.OCITokenSecret()
	if len(secret) == 0 {
		httpx.WriteServerProblem(
			w,
			http.StatusServiceUnavailable,
			"oci_token_unavailable",
			"OCI token signing is not configured",
			nil,
		)
		return
	}

	// Registry access tokens authorize registry requests only. Exchanging one
	// here would extend its lifetime after the primary credential was revoked
	// or expired, and would reuse external roles without consulting the IdP.
	// Check before authentication so a rate-limited bearer cannot fall back to
	// anonymous privileges and receive a fresh token.
	if isOCIAccessTokenAuthorization(r.Header.Get("Authorization")) {
		s.ChallengeOCIAuthentication(w, r, repository)
		return
	}
	user, authenticated := s.Authenticate(r)
	requested := requestedOCITokenActions(r)
	granted := make([]string, 0, len(requested))
	for _, action := range requested {
		privilege := ociTokenPrivilege(repository.Name, action)
		if privilege == "" {
			continue
		}
		allowed, err := s.UserHasPrivilege(r, user, authenticated, privilege)
		if err != nil {
			httpx.WriteOCIInternalError(w, err)
			return
		}
		if allowed {
			granted = append(granted, action)
		}
	}
	if len(granted) == 0 {
		s.ChallengeOCIAuthentication(w, r, repository)
		return
	}
	if authenticated && !user.External && user.Identity == "" {
		s.ChallengeOCIAuthentication(w, r, repository)
		return
	}

	subject := "anonymous"
	roles := []string(nil)
	if authenticated && user.Username != "" {
		subject = user.Username
		if user.External {
			roles = append([]string{}, user.Roles...)
		}
	}
	token, err := signOCIAccessToken(secret, AccessToken{
		Repository:      repository.Name,
		Subject:         subject,
		AccountIdentity: user.Identity,
		ExpiresAt:       time.Now().UTC().Add(ociAccessTokenTTL).Unix(),
		Actions:         granted,
		Admin:           authenticated && user.Admin,
		Local:           authenticated && !user.External,
		External:        authenticated && user.External,
		Roles:           roles,
	})
	if err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"token":        token,
		"access_token": token,
		"expires_in":   int(ociAccessTokenTTL.Seconds()),
		"issued_at":    time.Now().UTC().Format(time.RFC3339),
	})
}

func isOCIAccessTokenAuthorization(header string) bool {
	if strings.HasPrefix(header, ociAccessTokenPrefix) {
		return true
	}
	if len(header) >= len("bearer ") && strings.EqualFold(header[:len("bearer ")], "bearer ") {
		return strings.HasPrefix(strings.TrimSpace(header[len("bearer "):]), ociAccessTokenPrefix)
	}
	return false
}

func (s *Service) ChallengeOCIAuthentication(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
) {
	if len(s.OCITokenSecret()) > 0 {
		service := r.Host
		if service == "" {
			service = "suxen"
		}
		w.Header().Add(
			"WWW-Authenticate",
			fmt.Sprintf(
				`Bearer realm=%q,service=%q`,
				s.ociTokenRealm(r),
				service,
			),
		)
	}
	w.Header().Add("WWW-Authenticate", `Basic realm="suxen"`)
	httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", "authentication required")
}

func (s *Service) ociTokenRealm(r *http.Request) string {
	// The realm must be the same registry root the client already reached.
	// PublicURL is the control-plane/OIDC origin and can differ from extra
	// OCI hosts and listen ports.
	scheme := "http"
	if httpx.RequestUsesTLS(r) || ociTokenForwardedHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "suxen"
	}
	return scheme + "://" + host + TokenRealmPath(r.URL.Path)
}

func TokenRealmPath(requestPath string) string {
	if name, rest, ok := strings.Cut(strings.TrimPrefix(requestPath, "/repository/"), "/"); ok && name != "" {
		if rest == "v2" || strings.HasPrefix(rest, "v2/") {
			return "/repository/" + name + "/v2/token"
		}
	}
	return "/v2/token"
}

func ociTokenForwardedHTTPS(r *http.Request) bool {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

func (s *Service) OCITokenSecret() []byte {
	if len(s.Config.OIDCStateSecret) < 32 {
		return nil
	}
	return []byte(s.Config.OIDCStateSecret)
}

func (s *Service) authenticateOCIAccessToken(
	r *http.Request,
	token string,
) (domain.User, bool, string) {
	claims, err := verifyOCIAccessToken(s.OCITokenSecret(), token)
	if err != nil {
		s.recordAuthentication(r, false)
		return domain.User{}, false, "oci-token"
	}
	user := domain.User{
		Username:    claims.Subject,
		TokenScopes: ociTokenScopes(claims),
		Admin:       claims.Admin,
		External:    claims.External,
		Identity:    claims.AccountIdentity,
		Roles:       append([]string{}, claims.Roles...),
	}
	if !user.External && !claims.Local && (claims.Subject == "" || claims.Subject == "anonymous") {
		// Anonymous tokens must never inherit the privileges of a same-named
		// local account, including tokens issued before the Local discriminator.
		user.Username = ""
		user.Admin = false
	} else if !user.External {
		if claims.AccountIdentity == "" {
			s.recordAuthentication(r, false)
			return domain.User{}, false, "oci-token"
		}
		stored, err := s.meta().User(r.Context(), claims.Subject)
		if err != nil || stored.Identity != claims.AccountIdentity {
			s.recordAuthentication(r, false)
			return domain.User{}, false, "oci-token"
		}
		user.Admin = stored.Admin
	}
	s.recordAuthentication(r, true)
	recordAuthenticatedSubject(r, user, true)
	return user, true, "oci-token"
}

func requestedOCITokenActions(r *http.Request) []string {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
	}
	scopeValues := append([]string{}, r.URL.Query()["scope"]...)
	if r.PostForm != nil {
		scopeValues = append(scopeValues, r.PostForm["scope"]...)
	}
	seen := make(map[string]bool)
	actions := make([]string, 0)
	for _, scopeValue := range scopeValues {
		for _, scope := range strings.Fields(scopeValue) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) != 3 || parts[0] != "repository" {
				continue
			}
			for _, action := range strings.Split(parts[2], ",") {
				action = strings.TrimSpace(action)
				if action == "" || seen[action] || ociTokenPrivilege("", action) == "" {
					continue
				}
				seen[action] = true
				actions = append(actions, action)
			}
		}
	}
	if len(actions) == 0 {
		return []string{"pull"}
	}
	return actions
}

func ociTokenPrivilege(repositoryName string, action string) string {
	switch action {
	case "pull":
		return fmt.Sprintf("repository:%s:read", repositoryName)
	case "push":
		return fmt.Sprintf("repository:%s:write", repositoryName)
	case "delete":
		return fmt.Sprintf("repository:%s:delete", repositoryName)
	default:
		return ""
	}
}

func ociTokenScopes(claims AccessToken) []string {
	scopes := make([]string, 0, len(claims.Actions))
	for _, action := range claims.Actions {
		if privilege := ociTokenPrivilege(claims.Repository, action); privilege != "" {
			scopes = append(scopes, privilege)
		}
	}
	return scopes
}

func signOCIAccessToken(secret []byte, claims AccessToken) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode OCI access token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return ociAccessTokenPrefix + encoded + "." + ociTokenMAC(secret, encoded), nil
}

// SignAccessToken mints a registry bearer token.
func SignAccessToken(secret []byte, claims AccessToken) (string, error) {
	return signOCIAccessToken(secret, claims)
}

func verifyOCIAccessToken(secret []byte, token string) (AccessToken, error) {
	var claims AccessToken
	if len(secret) == 0 {
		return claims, fmt.Errorf("OCI token signing is not configured")
	}
	body, ok := strings.CutPrefix(token, ociAccessTokenPrefix)
	if !ok {
		return claims, fmt.Errorf("not an OCI access token")
	}
	encoded, mac, ok := strings.Cut(body, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(ociTokenMAC(secret, encoded))) {
		return claims, fmt.Errorf("OCI access token signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return claims, fmt.Errorf("decode OCI access token: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("decode OCI access token: %w", err)
	}
	if claims.Repository == "" || time.Now().UTC().Unix() >= claims.ExpiresAt {
		return claims, fmt.Errorf("OCI access token is expired")
	}
	return claims, nil
}

func ociTokenMAC(secret []byte, encodedPayload string) string {
	signature := hmac.New(sha256.New, secret)
	_, _ = signature.Write([]byte("suxen-oci-access-v1|"))
	_, _ = signature.Write([]byte(encodedPayload))
	return base64.RawURLEncoding.EncodeToString(signature.Sum(nil))
}
