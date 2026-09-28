package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestProxyQueryCredentialsAreRedactedFromErrorsAndRepositoryResponses(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const secret = "review-secret-sentinel"
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "signed-upstream", Format: "raw", Type: "proxy",
		Upstream: "https://upstream.example/artifacts?token=" + secret,
	}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	receivedQueryCredential := false
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			receivedQueryCredential = request.URL.Query().Get("token") == secret
			return nil, errors.New("connection failure")
		},
	)})
	response := fixture.request(
		t, http.MethodGet, "/repository/signed-upstream/package.bin", nil, true,
	)
	response.Body.Close()
	if !receivedQueryCredential {
		t.Fatal("configured upstream query credential was not sent to the upstream")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("transport error log leaked upstream query credential: %s", logs.String())
	}

	if err := fixture.Metadata.UpdateRole(context.Background(), domain.Role{
		Name: "anonymous", Privileges: []string{"repository:signed-upstream:read"},
	}); err != nil {
		t.Fatal(err)
	}
	metadata := fixture.request(
		t, http.MethodGet, "/api/v1/repositories/signed-upstream", nil, false,
	)
	defer metadata.Body.Close()
	body, err := io.ReadAll(metadata.Body)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.StatusCode != http.StatusOK {
		t.Fatalf("repository response: %d %s", metadata.StatusCode, body)
	}
	if strings.Contains(string(body), secret) || strings.Contains(string(body), "token=") {
		t.Fatalf("repository response leaked upstream query credential: %s", body)
	}
	if !strings.Contains(string(body), `"upstream":"https://upstream.example/artifacts"`) {
		t.Fatalf("repository response lost redacted upstream coordinates: %s", body)
	}
}

func TestProxyRedirectQueryCredentialsAreRedactedFromTransportErrors(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const secret = "review-redirect-secret"
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "redirected-upstream", Format: "raw", Type: "proxy",
		Upstream: "https://upstream.example/artifacts",
	}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			if request.URL.Host == "upstream.example" {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header: http.Header{"Location": []string{
						"https://cdn.example/artifact?X-Amz-Signature=" + secret,
					}},
					Body: io.NopCloser(strings.NewReader("")), Request: request,
				}, nil
			}
			return nil, errors.New("simulated CDN timeout")
		},
	)})

	response := fixture.request(
		t, http.MethodGet, "/repository/redirected-upstream/artifact", nil, true,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	logged := logs.String()
	if strings.Contains(logged, secret) || strings.Contains(logged, "X-Amz-Signature") {
		t.Fatalf("redirect transport error leaked presigned URL: %s", logged)
	}
	if !strings.Contains(logged, "cdn.example/artifact") ||
		!strings.Contains(logged, "simulated CDN timeout") {
		t.Fatalf("redaction removed useful redirect diagnostics: %s", logged)
	}
}

func TestProxyDoesNotSendBasicCredentialsToUnlistedTokenRealm(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	var authorizationHeaders []string
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			authorizationHeaders = append(
				authorizationHeaders,
				request.Header.Get("Authorization"),
			)
			return testHTTPResponse(request, http.StatusOK, `{"token":"test-token"}`), nil
		}),
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	challenge := content.BearerChallenge{Realm: "https://tokens.example/token"}
	credentials := content.UpstreamCredentials{Username: "registry-user", Password: "registry-secret"}

	if _, err := fixture.Handler.content.FetchBearerToken(
		request,
		"https://registry.example/v2/image/manifests/latest",
		challenge,
		credentials,
	); err != nil {
		t.Fatal(err)
	}
	if authorizationHeaders[0] != "" {
		t.Fatalf("cross-host realm received Authorization %q", authorizationHeaders[0])
	}

	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyRealmHosts = []string{"tokens.example"} })
	if _, err := fixture.Handler.content.FetchBearerToken(
		request,
		"https://registry.example/v2/image/manifests/latest",
		challenge,
		credentials,
	); err != nil {
		t.Fatal(err)
	}
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte("registry-user:registry-secret"),
	)
	if authorizationHeaders[1] != wantAuthorization {
		t.Fatalf(
			"allowlisted token realm received Authorization %q, want configured Basic credentials",
			authorizationHeaders[1],
		)
	}
}

func TestProxyRejectsMalformedTokenRealmBeforeRequest(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	requests := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			return testHTTPResponse(request, http.StatusOK, `{"token":"test-token"}`), nil
		}),
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := fixture.Handler.content.FetchBearerToken(
		request,
		"https://registry.example/v2/",
		content.BearerChallenge{Realm: "https://attacker:secret@tokens.example/token"},
		content.UpstreamCredentials{Username: "registry-user", Password: "registry-secret"},
	)
	if err == nil || !strings.Contains(err.Error(), "must not contain user information") {
		t.Fatalf("got error %v, want token realm validation error", err)
	}
	if requests != 0 {
		t.Fatalf("made %d token requests for an invalid realm", requests)
	}
}

func TestProxyRejectsTokenRealmSecurityDowngrade(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	requests := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			return testHTTPResponse(request, http.StatusOK, `{"token":"test-token"}`), nil
		}),
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	credentials := content.UpstreamCredentials{Username: "registry-user", Password: "registry-secret"}

	_, err := fixture.Handler.content.FetchBearerToken(
		request,
		"https://registry.example/v2/",
		content.BearerChallenge{Realm: "http://registry.example/token"},
		credentials,
	)
	if err == nil || !strings.Contains(err.Error(), "must not downgrade") {
		t.Fatalf("got error %v, want token realm downgrade rejection", err)
	}
	if requests != 0 {
		t.Fatalf("made %d requests to an insecure token realm", requests)
	}
}

func TestWebhookDeliveryBlocksLoopbackTarget(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	delivery := domain.WebhookDelivery{
		ID:        42,
		Event:     domain.WebhookAssetUploaded,
		TargetURL: "http://127.0.0.1:1/internal",
		Payload:   []byte(`{"event":"asset.uploaded"}`),
		Secret:    "webhook-secret",
	}

	err := fixture.Handler.content.SendWebhookRequest(context.Background(), delivery)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("got webhook error %v, want outbound policy rejection", err)
	}
}
