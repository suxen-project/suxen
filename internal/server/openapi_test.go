package server

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/contract"
)

func TestAPIDiscoveryAdvertisesStableMounts(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1", nil, false)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()

	var discovery apiDiscoveryResponse
	if err := json.NewDecoder(response.Body).Decode(&discovery); err != nil {
		t.Fatal(err)
	}
	if len(discovery.Versions) != 1 {
		t.Fatalf("version count = %d, want 1", len(discovery.Versions))
	}
	version := discovery.Versions[0]
	if version.Name != "v1" || version.Path != "/api/v1" || version.OpenAPI != "/api/openapi.json" {
		t.Fatalf("unexpected version discovery: %+v", version)
	}
	if version.Version != contract.HTTPAPIVersion() {
		t.Fatalf("discovery http-api version = %q, want %q", version.Version, contract.HTTPAPIVersion())
	}
	if len(discovery.Contract) == 0 {
		t.Fatal("discovery contract matrix is empty")
	}
	if discovery.Mounts.DefaultOCI != "/v2/" {
		t.Fatalf("default OCI mount = %q", discovery.Mounts.DefaultOCI)
	}
	if discovery.Mounts.PluginRoutes != "/api/v1/plugins/{pluginID}/" {
		t.Fatalf("plugin route mount = %q", discovery.Mounts.PluginRoutes)
	}
	if discovery.Mounts.Repositories != "/repository/{name}/" {
		t.Fatalf("repository mount = %q", discovery.Mounts.Repositories)
	}
	if len(discovery.Formats) == 0 {
		t.Fatal("discovery formats are empty")
	}
	for _, name := range []string{"raw", "oci"} {
		if !slices.Contains(discovery.Formats, name) {
			t.Fatalf("discovery formats = %v, missing %q", discovery.Formats, name)
		}
	}
	if !slices.Equal(discovery.AttributePaths, assetattrs.KnownAttributePaths()) {
		t.Errorf("discovery attribute paths = %v, want authoritative catalogue %v", discovery.AttributePaths, assetattrs.KnownAttributePaths())
	}
	for _, name := range discovery.Formats {
		if !assetattrs.IsReservedNamespace(name) {
			t.Errorf("discovered format %q is not a reserved attribute namespace", name)
		}
	}
	if discovery.Plugins == nil {
		t.Fatal("discovery plugins is nil")
	}
}

func TestOpenAPIAdvertisesControlPlaneCRUD(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/openapi.json", nil, false)
	assertStatus(t, response, http.StatusOK)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	document := string(body)
	paths := []string{
		`/api/v1/repositories/{name}/assets/{id}`,
		`/api/v1/repositories/{name}/assets/{id}/attributes/{namespace}`,
		`/api/v1/users/{name}`,
		`/api/v1/users/{name}/tokens/{id}`,
		`/api/v1/cleanup-policies/{name}`,
		`/api/v1/oidc-providers/{name}`,
		`/api/v1/webhooks/{name}`,
	}
	for _, path := range paths {
		if !strings.Contains(document, path) {
			t.Errorf("OpenAPI document does not advertise %s", path)
		}
	}
}
