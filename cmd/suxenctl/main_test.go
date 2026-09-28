package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobStoreCommandRoutes(t *testing.T) {
	configurationFile := filepath.Join(t.TempDir(), "blobstore.json")
	configuration := `{
        "name": "cold storage",
        "driver": "s3",
        "configurationRef": {"env": "SUXEN_COLD_STORAGE"}
    }`
	if err := os.WriteFile(configurationFile, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		arguments  []string
		wantMethod string
		wantPath   string
		wantBody   bool
	}{
		{
			name:       "list",
			arguments:  []string{"list"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/blob-stores",
		},
		{
			name:       "get",
			arguments:  []string{"get", "cold storage"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/blob-stores/cold%20storage",
		},
		{
			name:       "create",
			arguments:  []string{"create", configurationFile},
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/blob-stores",
			wantBody:   true,
		},
		{
			name:       "update",
			arguments:  []string{"update", "cold storage", configurationFile},
			wantMethod: http.MethodPut,
			wantPath:   "/api/v1/blob-stores/cold%20storage",
			wantBody:   true,
		},
		{
			name:       "delete",
			arguments:  []string{"delete", "cold storage"},
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/blob-stores/cold%20storage",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body []byte
				var err error
				if r.Body != nil {
					body, err = io.ReadAll(r.Body)
				}
				received = recordedRequest{
					method:        r.Method,
					path:          r.URL.EscapedPath(),
					authorization: r.Header.Get("Authorization"),
					body:          body,
					err:           err,
				}
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Status:     "204 No Content",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			})

			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}
			if err := blobStoreCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			if received.err != nil {
				t.Fatal(received.err)
			}
			if received.method != test.wantMethod {
				t.Errorf("method = %s, want %s", received.method, test.wantMethod)
			}
			if received.path != test.wantPath {
				t.Errorf("path = %s, want %s", received.path, test.wantPath)
			}
			if received.authorization != "Bearer test-token" {
				t.Errorf("Authorization = %q", received.authorization)
			}
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(configuration))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
		})
	}
}

func TestOIDCProviderCommandRoutes(t *testing.T) {
	configurationFile := filepath.Join(t.TempDir(), "oidc-provider.json")
	configuration := `{
        "name": "workforce login",
        "issuer": "https://identity.example.test",
        "clientId": "suxen",
        "clientSecret": "secret"
    }`
	if err := os.WriteFile(configurationFile, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		arguments  []string
		wantMethod string
		wantPath   string
		wantBody   bool
		response   string
	}{
		{
			name:       "list",
			arguments:  []string{"list"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/oidc-providers",
			response:   `{"items":[],"nextCursor":""}`,
		},
		{
			name:       "get",
			arguments:  []string{"get", "workforce login"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/oidc-providers/workforce%20login",
			response:   `{}`,
		},
		{
			name:       "create",
			arguments:  []string{"create", configurationFile},
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/oidc-providers",
			wantBody:   true,
			response:   `{}`,
		},
		{
			name:       "update",
			arguments:  []string{"update", "workforce login", configurationFile},
			wantMethod: http.MethodPut,
			wantPath:   "/api/v1/oidc-providers/workforce%20login",
			wantBody:   true,
			response:   `{}`,
		},
		{
			name:       "delete",
			arguments:  []string{"delete", "workforce login"},
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/oidc-providers/workforce%20login",
			response:   `{}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := readRequestBody(request)
				received = recordedRequest{
					method:        request.Method,
					path:          request.URL.EscapedPath(),
					authorization: request.Header.Get("Authorization"),
					body:          body,
					err:           err,
				}
				return commandResponse(request, test.response), nil
			})
			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}

			if err := oidcProviderCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			assertRecordedRequest(t, received, test.wantMethod, test.wantPath)
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(configuration))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
		})
	}
}

func TestDownloadGateDefaultsCommandRoutes(t *testing.T) {
	gateFile := filepath.Join(t.TempDir(), "defaults.json")
	gate := `{"criteria":[{"path":"scan.status","op":"=","value":"passed"}],"enabled":true}`
	if err := os.WriteFile(gateFile, []byte(gate), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		arguments  []string
		wantMethod string
		wantBody   bool
	}{
		{name: "get", arguments: []string{"defaults", "get"}, wantMethod: http.MethodGet},
		{name: "set", arguments: []string{"defaults", "set", gateFile}, wantMethod: http.MethodPut, wantBody: true},
		{name: "delete", arguments: []string{"defaults", "delete"}, wantMethod: http.MethodDelete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := readRequestBody(request)
				received = recordedRequest{
					method:        request.Method,
					path:          request.URL.EscapedPath(),
					authorization: request.Header.Get("Authorization"),
					body:          body,
					err:           err,
				}
				return commandResponse(request, `{}`), nil
			})
			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}
			if err := downloadGateCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			assertRecordedRequest(t, received, test.wantMethod, "/api/v1/download-gate-defaults")
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(gate))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
		})
	}
}

func TestClassificationDefaultsCommandRoutes(t *testing.T) {
	rulesFile := filepath.Join(t.TempDir(), "defaults.json")
	rules := `{"rules":[{"when":[],"key":"tier","value":"public"}]}`
	if err := os.WriteFile(rulesFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		arguments  []string
		wantMethod string
		wantBody   bool
	}{
		{name: "get", arguments: []string{"defaults", "get"}, wantMethod: http.MethodGet},
		{name: "set", arguments: []string{"defaults", "set", rulesFile}, wantMethod: http.MethodPut, wantBody: true},
		{name: "delete", arguments: []string{"defaults", "delete"}, wantMethod: http.MethodDelete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := readRequestBody(request)
				received = recordedRequest{
					method:        request.Method,
					path:          request.URL.EscapedPath(),
					authorization: request.Header.Get("Authorization"),
					body:          body,
					err:           err,
				}
				return commandResponse(request, `{}`), nil
			})
			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}
			if err := classificationCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			assertRecordedRequest(t, received, test.wantMethod, "/api/v1/classification-defaults")
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(rules))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
		})
	}
}

func TestTrustPolicyDefaultsCommandRoutes(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "defaults.json")
	policy := `{"mode":"audit","publicKeys":["k"]}`
	if err := os.WriteFile(policyFile, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		arguments  []string
		wantMethod string
		wantBody   bool
	}{
		{name: "get", arguments: []string{"defaults", "get"}, wantMethod: http.MethodGet},
		{name: "set", arguments: []string{"defaults", "set", policyFile}, wantMethod: http.MethodPut, wantBody: true},
		{name: "delete", arguments: []string{"defaults", "delete"}, wantMethod: http.MethodDelete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := readRequestBody(request)
				received = recordedRequest{
					method:        request.Method,
					path:          request.URL.EscapedPath(),
					authorization: request.Header.Get("Authorization"),
					body:          body,
					err:           err,
				}
				return commandResponse(request, `{}`), nil
			})
			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}
			if err := trustPolicyCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			assertRecordedRequest(t, received, test.wantMethod, "/api/v1/trust-policy-defaults")
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(policy))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
		})
	}
}

func TestAssetAttributeCommandRoutes(t *testing.T) {
	attributeFile := filepath.Join(t.TempDir(), "scan.json")
	attribute := `{"status":"passed","scanner":"trivy"}`
	if err := os.WriteFile(attributeFile, []byte(attribute), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		arguments   []string
		wantMethod  string
		wantBody    bool
		wantIfMatch string
	}{
		{
			name:       "get",
			arguments:  []string{"get", "release repo", "42", "scanner output"},
			wantMethod: http.MethodGet,
		},
		{
			name:        "set",
			arguments:   []string{"set", "--if-match", "sha256:current", "release repo", "42", "scanner output", attributeFile},
			wantMethod:  http.MethodPut,
			wantBody:    true,
			wantIfMatch: "sha256:current",
		},
		{
			name:        "delete",
			arguments:   []string{"delete", "--if-match", "sha256:current", "release repo", "42", "scanner output"},
			wantMethod:  http.MethodDelete,
			wantIfMatch: "sha256:current",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received recordedRequest
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := readRequestBody(request)
				received = recordedRequest{
					method:        request.Method,
					path:          request.URL.EscapedPath(),
					authorization: request.Header.Get("Authorization"),
					ifMatch:       request.Header.Get("If-Match"),
					body:          body,
					err:           err,
				}
				return commandResponse(request, `{}`), nil
			})
			api := &client{
				baseURL: "http://suxen.test",
				token:   "test-token",
				http:    &http.Client{Transport: transport},
			}

			if err := assetAttributeCommand(api, test.arguments); err != nil {
				t.Fatal(err)
			}
			assertRecordedRequest(
				t,
				received,
				test.wantMethod,
				"/api/v1/repositories/release%20repo/assets/42/attributes/scanner%20output",
			)
			if test.wantBody {
				assertEquivalentJSON(t, received.body, []byte(attribute))
			} else if len(received.body) != 0 {
				t.Errorf("unexpected request body: %s", received.body)
			}
			if received.ifMatch != test.wantIfMatch {
				t.Errorf("If-Match = %q, want %q", received.ifMatch, test.wantIfMatch)
			}
		})
	}
}

func TestWhoamiCommandRoute(t *testing.T) {
	var received recordedRequest
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		received = recordedRequest{
			method:        request.Method,
			path:          request.URL.EscapedPath(),
			authorization: request.Header.Get("Authorization"),
		}
		return commandResponse(request, `{"username":"operator"}`), nil
	})
	api := &client{
		baseURL: "http://suxen.test",
		token:   "test-token",
		http:    &http.Client{Transport: transport},
	}

	if err := whoamiCommand(api, nil); err != nil {
		t.Fatal(err)
	}
	assertRecordedRequest(t, received, http.MethodGet, "/api/v1/whoami")
	if err := whoamiCommand(api, []string{"extra"}); err == nil {
		t.Fatal("whoami with an argument unexpectedly succeeded")
	}
}

func TestDispatchUsesCanonicalResourceNouns(t *testing.T) {
	api := &client{http: http.DefaultClient}
	canonical := []string{
		"attribute",
		"blob-store",
		"cleanup-policy",
		"download-gate",
		"oidc-provider",
		"trust-policy",
	}
	for _, command := range canonical {
		t.Run(command, func(t *testing.T) {
			err := dispatch(api, []string{command})
			if err == nil || strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("canonical command error = %v", err)
			}
		})
	}

}

func TestApplyCommandResolvesSecretsLocally(t *testing.T) {
	t.Setenv("SUXENCTL_TEST_PASSWORD", "resolved-password")
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	document := `
resources:
  - kind: user
    name: automation
    spec:
      admin: false
      secretRef:
        env: SUXENCTL_TEST_PASSWORD
`
	if err := os.WriteFile(documentPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	var received recordedRequest
	var rawQuery string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		received = recordedRequest{
			method:        request.Method,
			path:          request.URL.EscapedPath(),
			authorization: request.Header.Get("Authorization"),
			body:          body,
			err:           err,
		}
		rawQuery = request.URL.RawQuery
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"dryRun":true,"prune":true,"results":[]}`)),
			Request:    request,
		}, nil
	})
	api := &client{
		baseURL: "http://localhost:8080",
		token:   "test-token",
		http:    &http.Client{Transport: transport},
	}
	if err := applyCommand(api, []string{"-f", documentPath, "--dry-run", "--prune"}); err != nil {
		t.Fatal(err)
	}
	if received.err != nil {
		t.Fatal(received.err)
	}
	if received.method != http.MethodPost || received.path != "/api/v1/provision" {
		t.Fatalf("request = %s %s", received.method, received.path)
	}
	if !strings.Contains(rawQuery, "dryRun=true") || !strings.Contains(rawQuery, "prune=true") {
		t.Fatalf("query = %q", rawQuery)
	}
	body := string(received.body)
	if !strings.Contains(body, `"password":"resolved-password"`) {
		t.Fatalf("resolved payload = %s", body)
	}
	if strings.Contains(body, "secretRef") || strings.Contains(body, "SUXENCTL_TEST_PASSWORD") {
		t.Fatalf("payload retained local secret reference: %s", body)
	}
}

func TestApplyCommandRejectsRemotePlaintextTransport(t *testing.T) {
	api := &client{baseURL: "http://suxen.example.test", token: "test-token", http: http.DefaultClient}
	err := applyCommand(api, []string{"-f", "desired-state.yaml"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("error = %v, want HTTPS requirement", err)
	}
}

func TestApplyCommandFailsOnHTTP200FailedReport(t *testing.T) {
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	if err := os.WriteFile(documentPath, []byte("resources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const report = `{"dryRun":true,"prune":false,"results":[{"kind":"role","name":"reader","status":"failed","error":"invalid role"}]}`
	api := &client{
		baseURL: "http://localhost:8080", token: "test-token",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(report)), Request: request}, nil
		})},
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previous })
	err = applyCommand(api, []string{"-f", documentPath, "--dry-run"})
	_ = writer.Close()
	printed, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || !strings.Contains(err.Error(), "1 resource") {
		t.Fatalf("apply error = %v, want failed resource summary", err)
	}
	if string(printed) != report {
		t.Fatalf("printed report = %q, want %q", printed, report)
	}
}

func TestApplyBinaryExitsNonzeroForHTTP200FailedReport(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "suxenctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build suxenctl: %v\n%s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/provision" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"dryRun":false,"prune":false,"results":[{"kind":"role","name":"reader","status":"failed"}]}`)
	}))
	defer server.Close()
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	if err := os.WriteFile(documentPath, []byte("resources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--url", server.URL, "--token", "test-token", "apply", "-f", documentPath)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("suxenctl exited 0: %s", output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1; output = %s", err, output)
	}
	if !strings.Contains(string(output), `"status":"failed"`) || !strings.Contains(string(output), "provisioning failed for 1 resource") {
		t.Fatalf("missing report or summary: %s", output)
	}
}

func TestApplyCommandRequiresAuthenticationBeforeReadingSecrets(t *testing.T) {
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	missingSecretPath := filepath.Join(t.TempDir(), "missing-secret")
	document := `
resources:
  - kind: user
    name: automation
    spec:
      secretRef:
        file: ` + missingSecretPath + `
`
	if err := os.WriteFile(documentPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	})
	api := &client{
		baseURL: "http://localhost:8080",
		http:    &http.Client{Transport: transport},
	}
	err := applyCommand(api, []string{"-f", documentPath})
	if err == nil || !strings.Contains(err.Error(), "authentication token") {
		t.Fatalf("error = %v, want authentication precondition", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no network access", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type recordedRequest struct {
	method        string
	path          string
	authorization string
	ifMatch       string
	body          []byte
	err           error
}

func readRequestBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	return io.ReadAll(request.Body)
}

func commandResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func assertRecordedRequest(
	t *testing.T,
	received recordedRequest,
	wantMethod string,
	wantPath string,
) {
	t.Helper()
	if received.err != nil {
		t.Fatal(received.err)
	}
	if received.method != wantMethod {
		t.Errorf("method = %s, want %s", received.method, wantMethod)
	}
	if received.path != wantPath {
		t.Errorf("path = %s, want %s", received.path, wantPath)
	}
	if received.authorization != "Bearer test-token" {
		t.Errorf("Authorization = %q", received.authorization)
	}
}

func TestBlobStoreCommandRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"list", "extra"},
		{"get"},
		{"create"},
		{"update", "archive"},
		{"delete"},
		{"unknown"},
	}
	api := &client{http: http.DefaultClient}
	for _, arguments := range tests {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			if err := blobStoreCommand(api, arguments); err == nil {
				t.Fatalf("arguments %q unexpectedly succeeded", arguments)
			}
		})
	}
}

func TestOIDCProviderCommandRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"list", "extra"},
		{"get"},
		{"create"},
		{"update", "corporate"},
		{"delete"},
		{"unknown"},
	}
	api := &client{http: http.DefaultClient}
	for _, arguments := range tests {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			if err := oidcProviderCommand(api, arguments); err == nil {
				t.Fatalf("arguments %q unexpectedly succeeded", arguments)
			}
		})
	}
}

func TestAssetAttributeCommandRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"get", "raw", "42"},
		{"get", "raw", "invalid", "scan"},
		{"get", "raw", "0", "scan"},
		{"get", "raw", "42", ""},
		{"set", "raw", "42", "scan"},
		{"set", "raw", "42", "scan", "scan.json"},
		{"delete", "raw", "42"},
		{"delete", "raw", "42", "scan"},
		{"unknown"},
	}
	api := &client{http: http.DefaultClient}
	for _, arguments := range tests {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			if err := assetAttributeCommand(api, arguments); err == nil {
				t.Fatalf("arguments %q unexpectedly succeeded", arguments)
			}
		})
	}
}

func TestWebhookApplyCommandRoute(t *testing.T) {
	configurationFile := filepath.Join(t.TempDir(), "webhook.json")
	configuration := `{
        "name": "ignored-body-name",
        "url": "https://scanner.example/hooks",
        "secret": "webhook-signing-secret",
        "events": ["asset.uploaded"],
        "enabled": true
    }`
	if err := os.WriteFile(configurationFile, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	var received recordedRequest
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		received = recordedRequest{
			method:        request.Method,
			path:          request.URL.EscapedPath(),
			authorization: request.Header.Get("Authorization"),
			body:          body,
			err:           err,
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"name":"scanner production"}`)),
			Request:    request,
		}, nil
	})
	api := &client{
		baseURL: "http://suxen.test",
		token:   "test-token",
		http:    &http.Client{Transport: transport},
	}

	if err := webhookCommand(api, []string{"apply", "scanner production", configurationFile}); err != nil {
		t.Fatal(err)
	}
	if received.err != nil {
		t.Fatal(received.err)
	}
	if received.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", received.method)
	}
	if received.path != "/api/v1/webhooks/scanner%20production" {
		t.Errorf("path = %s", received.path)
	}
	if received.authorization != "Bearer test-token" {
		t.Errorf("Authorization = %q", received.authorization)
	}
	assertEquivalentJSON(t, received.body, []byte(configuration))
}

func TestWebhookApplyCommandRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"apply"},
		{"apply", "scanner"},
		{"apply", "scanner", "one.json", "extra"},
	}
	api := &client{http: http.DefaultClient}
	for _, arguments := range tests {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			if err := webhookCommand(api, arguments); err == nil {
				t.Fatalf("arguments %q unexpectedly succeeded", arguments)
			}
		})
	}
}

func assertEquivalentJSON(t *testing.T, actual []byte, expected []byte) {
	t.Helper()
	var actualValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatalf("decode actual JSON: %v", err)
	}
	var expectedValue any
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	actualJSON, err := json.Marshal(actualValue)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(expectedValue)
	if err != nil {
		t.Fatal(err)
	}
	if string(actualJSON) != string(expectedJSON) {
		t.Fatalf("JSON body = %s, want %s", actualJSON, expectedJSON)
	}
}
