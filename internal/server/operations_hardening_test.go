package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

func TestAccessLogIncludesRequestIdentityAndRepositoryContext(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	request := httptest.NewRequest(
		http.MethodPut,
		"/repository/raw/releases/audit.bin",
		strings.NewReader("audit payload"),
	)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set(httpx.RequestIDHeader, "correlation-123")
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", response.Code)
	}
	if response.Header().Get(httpx.RequestIDHeader) != "correlation-123" {
		t.Fatalf("response request ID = %q", response.Header().Get(httpx.RequestIDHeader))
	}
	record := decodeSingleLogRecord(t, logs.String())
	assertLogField(t, record, "msg", "request")
	assertLogField(t, record, "request_id", "correlation-123")
	assertLogField(t, record, "subject", "admin")
	assertLogField(t, record, "repository", "raw")
	assertLogField(t, record, "format", "raw")
	assertLogField(t, record, "method", http.MethodPut)
	assertLogField(t, record, "path", "/repository/raw/releases/audit.bin")
}

func TestAccessLogSuppressesProbeAndScrapePaths(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	for _, requestPath := range []string{"/healthz", "/readyz", "/metrics"} {
		request := httptest.NewRequest(http.MethodGet, requestPath, nil)
		if requestPath == "/metrics" {
			request.Header.Set("Authorization", "Bearer "+testToken)
		}
		response := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", requestPath, response.Code)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("probe or scrape emitted access logs: %s", logs.String())
	}
}

func TestOperationalEndpointsRejectMutatingMethods(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	tests := []struct {
		path          string
		authenticated bool
	}{
		{path: "/healthz"},
		{path: "/readyz"},
		{path: "/metrics", authenticated: true},
		{path: "/version"},
		{path: "/api/openapi.json"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := fixture.request(
				t,
				http.MethodPost,
				test.path,
				nil,
				test.authenticated,
			)
			assertStatus(t, response, http.StatusMethodNotAllowed)
			defer response.Body.Close()
			if allow := response.Header.Get("Allow"); allow != "GET, HEAD" {
				t.Fatalf("Allow = %q, want GET, HEAD", allow)
			}
		})
	}
}

func TestOperationalEndpointsAllowHead(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	tests := []struct {
		path          string
		authenticated bool
	}{
		{path: "/healthz"},
		{path: "/readyz"},
		{path: "/metrics", authenticated: true},
		{path: "/version"},
		{path: "/api/openapi.json"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := fixture.request(
				t,
				http.MethodHead,
				test.path,
				nil,
				test.authenticated,
			)
			assertStatus(t, response, http.StatusOK)
			response.Body.Close()
		})
	}
}

func TestVersionIsNotDisclosedOnUnrelatedResponses(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/healthz", nil, false)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()

	if version := response.Header.Get("X-Suxen-Version"); version != "" {
		t.Fatalf("X-Suxen-Version = %q, want no global version header", version)
	}
}

func TestReadinessIgnoresBlobStoreAvailability(t *testing.T) {
	fixture := newServerFixture(t)
	t.Setenv("SUXEN_FAILING_STORE", "test://unavailable")
	if err := fixture.Metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "confidential-archive",
		Driver: "test",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_FAILING_STORE",
		},
		PhysicalIdentity: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.blobStores.Factory = func(string, string) (blob.Store, error) {
		return nil, errors.New("storage backend unavailable")
	}

	// A blob store being unavailable must not remove the pod from rotation: the
	// failure is per-request, and gating readiness on it would drop every replica
	// at once. Readiness depends only on metadata.
	response := fixture.request(t, http.MethodGet, "/readyz", nil, false)
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
}

func TestReadinessGateReportsInitializingWithoutRestartingHealth(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	// Default (post-startup) state: dependencies reachable, so the pod is ready.
	ready := fixture.request(t, http.MethodGet, "/readyz", nil, false)
	assertStatus(t, ready, http.StatusOK)
	ready.Body.Close()

	// While initializing, readiness reports 503 but liveness stays healthy, so
	// the kubelet keeps the pod running rather than restarting it.
	fixture.Handler.SetInitializing(true)
	initializing := fixture.request(t, http.MethodGet, "/readyz", nil, false)
	assertStatus(t, initializing, http.StatusServiceUnavailable)
	body, err := io.ReadAll(initializing.Body)
	initializing.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "initializing") {
		t.Fatalf("readiness response omitted the initializing reason: %s", body)
	}
	health := fixture.request(t, http.MethodGet, "/healthz", nil, false)
	assertStatus(t, health, http.StatusOK)
	health.Body.Close()

	// Once initialization completes, readiness recovers.
	fixture.Handler.SetInitializing(false)
	recovered := fixture.request(t, http.MethodGet, "/readyz", nil, false)
	assertStatus(t, recovered, http.StatusOK)
	recovered.Body.Close()
}

func TestAccessLogDoesNotTrustInvalidRequestIdentity(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	request := httptest.NewRequest(
		http.MethodPut,
		"/repository/raw/releases/rejected.bin",
		strings.NewReader("rejected payload"),
	)
	request.SetBasicAuth("claimed-administrator", "wrong-password")
	request.Header.Set(httpx.RequestIDHeader, "invalid request id")
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid credentials status = %d, want 401", response.Code)
	}
	generatedID := response.Header().Get(httpx.RequestIDHeader)
	if generatedID == "" || generatedID == "invalid request id" {
		t.Fatalf("unsafe request ID was accepted: %q", generatedID)
	}
	record := decodeSingleLogRecord(t, logs.String())
	assertLogField(t, record, "subject", "anonymous")
	if strings.Contains(logs.String(), "claimed-administrator") {
		t.Fatal("access log trusted the username from invalid credentials")
	}
}

func TestBootstrapMessageDoesNotExposeConfiguredCredentials(t *testing.T) {
	const password = "configured-bootstrap-password"
	const token = "configured-bootstrap-token-value"
	tests := map[string]struct {
		password        string
		token           string
		generatedLabels []string
	}{
		"both configured":    {password: password, token: token},
		"generated password": {token: token, generatedLabels: []string{"password="}},
		"generated token":    {password: password, generatedLabels: []string{"token="}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dataDirectory := t.TempDir()
			metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = metadata.Close() })
			blobStore, err := blob.NewFS(filepath.Join(dataDirectory, "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			handler := New(
				config.Config{
					DataDir:           dataDirectory,
					BlobURL:           "fs://" + filepath.Join(dataDirectory, "blobs"),
					BootstrapUser:     "admin",
					BootstrapPassword: test.password,
					BootstrapToken:    test.token,
				},
				metadata,
				blobStore,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			message, err := handler.Bootstrap(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(message, password) || strings.Contains(message, token) {
				t.Fatalf("bootstrap message exposed configured credentials: %s", message)
			}
			for _, label := range test.generatedLabels {
				if !strings.Contains(message, label) {
					t.Fatalf("bootstrap message %q is missing %q", message, label)
				}
			}
			if len(test.generatedLabels) == 0 && message != "" {
				t.Fatalf("configured credentials produced a one-time message: %s", message)
			}
		})
	}
}

func TestOperationalConfigurationWarnsWhenOIDCBrowserStateIsMissing(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateOIDCProvider(context.Background(), domain.OIDCProvider{
		Name:     "corporate",
		Issuer:   "https://identity.example",
		ClientID: "suxen",
	}); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.OIDCStateSecret = "" })
	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	if err := fixture.Handler.ValidateOperationalConfiguration(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := decodeSingleLogRecord(t, logs.String())
	assertLogField(t, record, "level", "WARN")
	assertLogField(t, record, "msg", "interactive OIDC login disabled")
	assertLogField(t, record, "reason", "SUXEN_OIDC_STATE_SECRET is not configured")
}

func TestSingleNodeScheduledCleanupDoesNotRequireALease(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Cluster = false })
	now := time.Now().UTC()
	acquired, err := fixture.Metadata.AcquireLease(
		context.Background(),
		cleanupLeaseName,
		"another-node",
		now,
		now.Add(time.Hour),
	)
	if err != nil || !acquired {
		t.Fatalf("reserve cleanup lease: acquired=%v err=%v", acquired, err)
	}

	fixture.Handler.runScheduledCleanup(context.Background())
	tasks, err := fixture.Metadata.Tasks(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("single-node cleanup unexpectedly ran another task: %+v", tasks)
	}

	fixture.Handler.runScheduledGarbageCollection(context.Background())
	tasks, err = fixture.Metadata.Tasks(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Type != "garbage-collection" {
		t.Fatalf("single-node garbage-collection tasks = %+v", tasks)
	}
}

func decodeSingleLogRecord(t *testing.T, output string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		t.Fatalf("decode log record: %v; output=%s", err, output)
	}
	var extra map[string]any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected one log record, got extra=%v err=%v", extra, err)
	}
	return record
}

func assertLogField(t *testing.T, record map[string]any, name string, expected any) {
	t.Helper()
	if record[name] != expected {
		t.Fatalf("log field %s = %#v, want %#v; record=%v", name, record[name], expected, record)
	}
}
