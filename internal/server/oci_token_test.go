package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/store"
)

func TestOCIPingAdvertisesBearerAndBasic(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	denied := fixture.request(t, http.MethodGet, "/v2/", nil, false)
	assertStatus(t, denied, http.StatusUnauthorized)
	assertOCIAuthChallenge(t, denied, `Bearer realm="http://suxen/v2/token"`, `Basic realm="suxen"`)
	denied.Body.Close()
}

func TestOCITokenRealmHonorsForwardedProto(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	request, err := http.NewRequest(http.MethodGet, "/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "registry.example.com"
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	denied := recorder.Result()
	assertStatus(t, denied, http.StatusUnauthorized)
	assertOCIAuthChallenge(
		t,
		denied,
		`Bearer realm="https://registry.example.com/v2/token"`,
		`Basic realm="suxen"`,
	)
	denied.Body.Close()
}

func TestOCITokenRestoresExternalRoles(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRole(context.Background(), domain.Role{
		Name:       "oci-publisher",
		Privileges: []string{"repository:oci:write"},
	}); err != nil {
		t.Fatal(err)
	}
	token, err := identity.SignAccessToken(
		[]byte(fixture.Handler.cfg.OIDCStateSecret),
		identity.AccessToken{
			Repository: "oci",
			Subject:    "oidc-publisher",
			ExpiresAt:  time.Now().UTC().Add(time.Minute).Unix(),
			Actions:    []string{"pull", "push"},
			External:   true,
			Roles:      []string{"oci-publisher"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	upload := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/v2/acme/app/blobs/uploads/",
		nil,
		"",
		token,
	)
	assertStatus(t, upload, http.StatusAccepted)
	upload.Body.Close()
}

func TestOCITokenAllowsAnonymousDockerPullHandshake(t *testing.T) {
	t.Parallel()
	fixture := newServerFixtureWithAnonymousRead(t)
	tokenResponse := fixture.request(t, http.MethodGet, "/v2/token", nil, false)
	assertStatus(t, tokenResponse, http.StatusOK)
	token := decodeOCIAccessToken(t, tokenResponse)
	if !strings.HasPrefix(token, identity.AccessTokenPrefix) {
		t.Fatalf("token %q is not an OCI access token", token)
	}

	ping := fixture.requestWithBearer(t, http.MethodGet, "/v2/", nil, "", token)
	assertStatus(t, ping, http.StatusOK)
	ping.Body.Close()

	content := []byte("anonymous-layer")
	digest := testDigest(content)
	upload := fixture.request(
		t,
		http.MethodPost,
		"/v2/acme/app/blobs/uploads/?digest="+digest,
		content,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	pulled := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/v2/acme/app/blobs/"+digest,
		nil,
		"",
		token,
	)
	assertStatus(t, pulled, http.StatusOK)
	assertBody(t, pulled, content)

	push := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/v2/acme/app/blobs/uploads/",
		nil,
		"",
		token,
	)
	assertStatus(t, push, http.StatusForbidden)
	push.Body.Close()
}

func TestOCITokenCannotRenewAfterPrimaryCredentialRevocation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	primary := "oci-temporary-api-token"
	record, err := fixture.Metadata.CreateToken(ctx, "admin", "oci-temporary", primary,
		[]string{"repository:oci:read", "repository:oci:write"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v2/token?scope=repository:acme/app:pull,push"
	issued := fixture.requestWithBearer(t, http.MethodGet, path, nil, "", primary)
	assertStatus(t, issued, http.StatusOK)
	derived := decodeOCIAccessToken(t, issued)

	if err := fixture.Metadata.DeleteToken(ctx, "admin", record.ID); err != nil {
		t.Fatal(err)
	}
	revoked := fixture.requestWithBearer(t, http.MethodGet, path, nil, "", primary)
	assertStatus(t, revoked, http.StatusUnauthorized)
	revoked.Body.Close()

	renewed := fixture.requestWithBearer(t, http.MethodGet, path, nil, "", derived)
	assertStatus(t, renewed, http.StatusUnauthorized)
	assertOCIAuthChallenge(t, renewed, `Bearer realm="http://suxen/v2/token"`, `Basic realm="suxen"`)
	renewed.Body.Close()
}

func TestOCITokenCannotExchangeDerivedBearerForAnonymousPull(t *testing.T) {
	t.Parallel()
	fixture := newServerFixtureWithAnonymousRead(t)
	path := "/v2/token?scope=repository:acme/app:pull"
	issued := fixture.request(t, http.MethodGet, path, nil, false)
	assertStatus(t, issued, http.StatusOK)
	derived := decodeOCIAccessToken(t, issued)

	renewed := fixture.requestWithBearer(t, http.MethodGet, path, nil, "", derived)
	assertStatus(t, renewed, http.StatusUnauthorized)
	renewed.Body.Close()

	bareRequest := httptest.NewRequest(http.MethodGet, path, nil)
	bareRequest.Header.Set("Authorization", derived)
	bareRecorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(bareRecorder, bareRequest)
	bareResponse := bareRecorder.Result()
	assertStatus(t, bareResponse, http.StatusUnauthorized)
	bareResponse.Body.Close()

	expired, err := identity.SignAccessToken(
		[]byte(fixture.Handler.cfg.OIDCStateSecret),
		identity.AccessToken{
			Repository: "oci",
			Subject:    "anonymous",
			ExpiresAt:  time.Now().UTC().Add(-time.Minute).Unix(),
			Actions:    []string{"pull"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	renewed = fixture.requestWithBearer(t, http.MethodGet, path, nil, "", expired)
	assertStatus(t, renewed, http.StatusUnauthorized)
	renewed.Body.Close()

	// A registry can still start a fresh anonymous handshake without a bearer.
	fresh := fixture.request(t, http.MethodGet, path, nil, false)
	assertStatus(t, fresh, http.StatusOK)
	fresh.Body.Close()

	fixture.Handler.identity.ReplaceFailureLimiter(1, time.Minute, 5*time.Minute)
	bad := fixture.requestWithBearer(t, http.MethodGet, "/api/v1/stats", nil, "", "invalid-primary-token")
	assertStatus(t, bad, http.StatusUnauthorized)
	bad.Body.Close()
	renewed = fixture.requestWithBearer(t, http.MethodGet, path, nil, "", derived)
	assertStatus(t, renewed, http.StatusUnauthorized)
	renewed.Body.Close()
}

func TestOCITokenRejectsInvalidBearer(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	denied := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/v2/",
		nil,
		"",
		identity.AccessTokenPrefix+"not-a-token",
	)
	assertStatus(t, denied, http.StatusUnauthorized)
	denied.Body.Close()
}

func TestOCITokenPreservesAdministratorPush(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateUser(
		context.Background(),
		"ops",
		"ops-password-12",
		true,
	); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodGet,
		"/v2/token?scope=repository:acme/app:pull,push",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("ops", "ops-password-12")
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	tokenResponse := recorder.Result()
	assertStatus(t, tokenResponse, http.StatusOK)
	token := decodeOCIAccessToken(t, tokenResponse)

	upload := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/v2/acme/app/blobs/uploads/",
		nil,
		"",
		token,
	)
	assertStatus(t, upload, http.StatusAccepted)
	upload.Body.Close()
}

func TestOCITokenDropsAdminAfterUserDeletion(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateUser(ctx, "ops", "ops-password-12", true); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodGet,
		"/v2/token?scope=repository:acme/app:pull,push",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("ops", "ops-password-12")
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	tokenResponse := recorder.Result()
	assertStatus(t, tokenResponse, http.StatusOK)
	token := decodeOCIAccessToken(t, tokenResponse)
	if err := fixture.Metadata.DeleteUser(ctx, "ops", store.Ownership{}); err != nil {
		t.Fatal(err)
	}

	upload := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/v2/acme/app/blobs/uploads/",
		nil,
		"",
		token,
	)
	assertStatus(t, upload, http.StatusUnauthorized)
	upload.Body.Close()
}

func TestOCITokenPathPrefixAndPushScope(t *testing.T) {
	t.Parallel()
	fixture := newServerFixtureWithAnonymousRead(t)
	anonymous := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci/v2/token?scope=repository:acme/app:pull,push",
		nil,
		false,
	)
	assertStatus(t, anonymous, http.StatusOK)
	pullOnly := decodeOCIAccessToken(t, anonymous)

	pushDenied := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/repository/oci/v2/acme/app/blobs/uploads/",
		nil,
		"",
		pullOnly,
	)
	assertStatus(t, pushDenied, http.StatusForbidden)
	pushDenied.Body.Close()

	granted := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci/v2/token?scope=repository:acme/app:pull,push",
		nil,
		true,
	)
	assertStatus(t, granted, http.StatusOK)
	pushToken := decodeOCIAccessToken(t, granted)
	upload := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/repository/oci/v2/acme/app/blobs/uploads/",
		nil,
		"",
		pushToken,
	)
	assertStatus(t, upload, http.StatusAccepted)
	upload.Body.Close()
}

func TestOCITokenUnavailableWithoutSigningSecret(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.OIDCStateSecret = "" })
	denied := fixture.request(t, http.MethodGet, "/v2/", nil, false)
	assertStatus(t, denied, http.StatusUnauthorized)
	assertOCIAuthChallenge(t, denied, `Basic realm="suxen"`)
	if challenges := denied.Header.Values("WWW-Authenticate"); len(challenges) != 1 {
		t.Fatalf("WWW-Authenticate = %v, want Basic only", challenges)
	}
	denied.Body.Close()

	unavailable := fixture.request(t, http.MethodGet, "/v2/token", nil, false)
	assertStatus(t, unavailable, http.StatusServiceUnavailable)
	unavailable.Body.Close()
}

func TestOCITokenRealmPath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/v2/":                                 "/v2/token",
		"/v2/acme/app/blobs/sha256:abc":        "/v2/token",
		"/repository/docker/v2/":               "/repository/docker/v2/token",
		"/repository/docker/v2/token":          "/repository/docker/v2/token",
		"/repository/docker/v2/acme/tags/list": "/repository/docker/v2/token",
		"/api/v1/repositories":                 "/v2/token",
	}
	for requestPath, want := range cases {
		if got := identity.TokenRealmPath(requestPath); got != want {
			t.Fatalf("identity.TokenRealmPath(%q) = %q, want %q", requestPath, got, want)
		}
	}
}

func assertOCIAuthChallenge(t *testing.T, response *http.Response, want ...string) {
	t.Helper()
	got := response.Header.Values("WWW-Authenticate")
	gotText := strings.Join(got, "\n")
	for _, challenge := range want {
		found := false
		for _, value := range got {
			if strings.Contains(value, challenge) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("WWW-Authenticate %q does not contain %q", gotText, challenge)
		}
	}
}

func decodeOCIAccessToken(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Token == "" || payload.Token != payload.AccessToken {
		t.Fatalf("token payload %+v", payload)
	}
	if payload.ExpiresIn != int(identity.AccessTokenTTL.Seconds()) {
		t.Fatalf("expires_in = %d", payload.ExpiresIn)
	}
	return payload.Token
}
