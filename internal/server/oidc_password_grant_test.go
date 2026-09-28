package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestOIDCPasswordGrantAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name:       "oidc-publisher",
		Privileges: []string{"repository:raw:write"},
	}); err != nil {
		t.Fatal(err)
	}

	createBody := []byte(`{
		"name":"internal",
		"issuer":"https://identity.example",
		"clientId":"suxen",
		"clientSecret":"server-side-secret",
		"allowPasswordGrant":true,
		"groupRoles":{"release-engineering":["oidc-publisher"]}
	}`)
	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/oidc-providers",
		createBody,
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const (
		serviceUser = "svc-account"
		servicePass = "correct-horse"
	)
	tokenRequests := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.String() {
			case "https://identity.example/.well-known/openid-configuration":
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"issuer":                 "https://identity.example",
					"authorization_endpoint": "https://identity.example/authorize",
					"token_endpoint":         "https://identity.example/token",
					"jwks_uri":               "https://identity.example/keys",
				}), nil
			case "https://identity.example/keys":
				return jsonHTTPResponse(request, http.StatusOK, testJWKS(&privateKey.PublicKey)), nil
			case "https://identity.example/token":
				tokenRequests++
				if err := request.ParseForm(); err != nil {
					return nil, err
				}
				if request.Form.Get("grant_type") != "password" ||
					request.Form.Get("username") != serviceUser ||
					request.Form.Get("password") != servicePass {
					return jsonHTTPResponse(request, http.StatusUnauthorized, map[string]any{
						"error": "invalid_grant",
					}), nil
				}
				idToken := signTestIDToken(t, privateKey, []string{"release-engineering"})
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"access_token": "opaque-access-token",
					"token_type":   "Bearer",
					"expires_in":   300,
					"id_token":     idToken,
				}), nil
			default:
				return nil, fmt.Errorf("unexpected OIDC request %s", request.URL)
			}
		}),
	})

	// docker login credentials against an internal provider: correct password
	// resolves through the password grant and the mapped role grants write.
	upload := fixture.requestWithBasic(
		t,
		http.MethodPut,
		"/repository/raw/from-password-grant.bin",
		[]byte("password-grant content"),
		"application/octet-stream",
		serviceUser,
		servicePass,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()
	if tokenRequests == 0 {
		t.Fatal("password grant never reached the provider token endpoint")
	}

	// Wrong password is rejected, not silently accepted.
	wrong := fixture.requestWithBasic(
		t,
		http.MethodPut,
		"/repository/raw/wrong-password.bin",
		[]byte("blocked"),
		"application/octet-stream",
		serviceUser,
		"wrong-password",
	)
	assertStatus(t, wrong, http.StatusUnauthorized)
	wrong.Body.Close()

	// Disabling the grant closes the path even for correct credentials.
	disableBody := []byte(`{
		"issuer":"https://identity.example",
		"clientId":"suxen",
		"allowPasswordGrant":false,
		"groupRoles":{"release-engineering":["oidc-publisher"]}
	}`)
	disabled := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/oidc-providers/internal",
		disableBody,
		"application/json",
		testToken,
	)
	assertStatus(t, disabled, http.StatusOK)
	disabled.Body.Close()

	tokenRequests = 0
	afterDisable := fixture.requestWithBasic(
		t,
		http.MethodPut,
		"/repository/raw/after-disable.bin",
		[]byte("blocked"),
		"application/octet-stream",
		serviceUser,
		servicePass,
	)
	assertStatus(t, afterDisable, http.StatusUnauthorized)
	afterDisable.Body.Close()
	if tokenRequests != 0 {
		t.Fatalf("disabled provider still attempted the password grant (%d calls)", tokenRequests)
	}
}

func (fixture *serverFixture) requestWithBasic(
	t *testing.T,
	method string,
	requestPath string,
	body []byte,
	contentType string,
	username string,
	password string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", contentType)
	}
	request.SetBasicAuth(username, password)

	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
