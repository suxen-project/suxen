package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestOCIRootRoutesByHostAndPort(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name:   "docker",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Hosts: []string{"registry.example.com"},
			Ports: []int{5000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name:   "images",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Hosts: []string{"images.example.com"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	layer := []byte("docker repository layer")
	layerDigest := testDigest(layer)
	upload := fixture.request(
		t,
		http.MethodPost,
		"/repository/docker/v2/acme/app/blobs/uploads/?digest="+layerDigest,
		layer,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	blobPath := "/v2/acme/app/blobs/" + layerDigest
	hostHit := fixture.requestOCIRoot(t, http.MethodGet, blobPath, "registry.example.com", 0, true)
	assertStatus(t, hostHit, http.StatusOK)
	hostHit.Body.Close()

	fallback := fixture.requestOCIRoot(t, http.MethodGet, blobPath, "", 0, true)
	assertStatus(t, fallback, http.StatusNotFound)
	fallback.Body.Close()

	portHit := fixture.requestOCIRoot(t, http.MethodGet, blobPath, "other.example.com", 5000, true)
	assertStatus(t, portHit, http.StatusOK)
	portHit.Body.Close()

	hostWins := fixture.requestOCIRoot(t, http.MethodGet, blobPath, "images.example.com", 5000, true)
	assertStatus(t, hostWins, http.StatusNotFound)
	hostWins.Body.Close()
}

func TestOCIEndpointConflictAndNonOCIRejection(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories",
		[]byte(`{
			"name":"docker",
			"format":"oci",
			"type":"hosted",
			"endpoints":{"hosts":["registry.example.com"],"ports":[5000]}
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	conflict := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories",
		[]byte(`{
			"name":"images",
			"format":"oci",
			"type":"hosted",
			"endpoints":{"hosts":["registry.example.com"]}
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, conflict, http.StatusConflict)
	conflict.Body.Close()

	raw := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories",
		[]byte(`{
			"name":"files",
			"format":"raw",
			"type":"hosted",
			"endpoints":{"ports":[5001]}
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, raw, http.StatusBadRequest)
	raw.Body.Close()
}

func TestExtraOCIListenerServesBoundRepository(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:   "docker",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Ports: []int{port},
		},
	}); err != nil {
		t.Fatal(err)
	}

	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Listen = "127.0.0.1:8080" })
	if err := fixture.Handler.StartExtraListeners(context.Background()); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(
		http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/v2/", port),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("extra listener /v2/ status %d, body %s", response.StatusCode, body)
	}

	control, err := http.NewRequest(
		http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/repositories", port),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	control.Header.Set("Authorization", "Bearer "+testToken)
	controlResponse, err := http.DefaultClient.Do(control)
	if err != nil {
		t.Fatal(err)
	}
	defer controlResponse.Body.Close()
	if controlResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("extra listener /api status %d, want 404", controlResponse.StatusCode)
	}
}

func TestOCIPingChallengesUnauthenticatedClients(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	denied := fixture.request(t, http.MethodGet, "/v2/", nil, false)
	assertStatus(t, denied, http.StatusUnauthorized)
	if denied.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("unauthenticated GET /v2/ omitted WWW-Authenticate")
	}
	denied.Body.Close()

	allowed := fixture.request(t, http.MethodGet, "/v2/", nil, true)
	assertStatus(t, allowed, http.StatusOK)
	allowed.Body.Close()
}

func TestCreateRepositorySucceedsWhenPortOccupied(t *testing.T) {
	t.Parallel()
	// Ports are bound at startup, so creating a repository on an already-taken
	// port succeeds and persists; the listener would only open on a restart.
	fixture := newServerFixture(t)
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	port := blocker.Addr().(*net.TCPAddr).Port

	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Listen = "127.0.0.1:8080" })
	if err := fixture.Handler.StartExtraListeners(context.Background()); err != nil {
		t.Fatal(err)
	}

	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories",
		[]byte(fmt.Sprintf(`{
			"name":"docker",
			"format":"oci",
			"type":"hosted",
			"endpoints":{"ports":[%d]}
		}`, port)),
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	if _, err := fixture.Metadata.Repository(context.Background(), "docker"); err != nil {
		t.Fatalf("created repository was not persisted: %v", err)
	}
}

func TestCreateRepositoryDoesNotBindPortLive(t *testing.T) {
	t.Parallel()
	// A repository created after startup does not open its listener live: the
	// configured port stays free until the next restart.
	fixture := newServerFixture(t)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}

	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Listen = "127.0.0.1:8080" })
	if err := fixture.Handler.StartExtraListeners(context.Background()); err != nil {
		t.Fatal(err)
	}

	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories",
		[]byte(fmt.Sprintf(`{
			"name":"docker",
			"format":"oci",
			"type":"hosted",
			"endpoints":{"ports":[%d]}
		}`, freePort)),
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	stillFree, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", freePort))
	if err != nil {
		t.Fatalf("create opened a listener live on port %d: %v", freePort, err)
	}
	_ = stillFree.Close()
}

func (fixture *serverFixture) requestOCIRoot(
	t *testing.T,
	method string,
	requestPath string,
	host string,
	port int,
	authenticated bool,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		request.Host = host
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+testToken)
	}
	request = requestWithListenPort(request, port)
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
