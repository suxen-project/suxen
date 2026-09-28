package contract

import (
	"slices"
	"testing"
)

// The embedded manifest is validated in init; these tests pin the invariants
// callers depend on and guard against silent surface deletion or renames.

func TestManifestDeclaresExpectedSurfaces(t *testing.T) {
	want := []string{
		SurfaceBlobstoreSPI,
		SurfaceFormatSPI,
		SurfacePluginAPISPI,
		SurfaceHTTPAPI,
		SurfaceWebhookAPI,
	}
	got := make([]string, 0, len(Surfaces()))
	for _, s := range Surfaces() {
		got = append(got, s.ID)
	}
	for _, id := range want {
		if !slices.Contains(got, id) {
			t.Fatalf("manifest is missing surface %q; declared = %v", id, got)
		}
	}
}

func TestHTTPAPIVersionMatchesPathMajor(t *testing.T) {
	version := HTTPAPIVersion()
	if version == "" {
		t.Fatal("http-api version is empty")
	}
	var path string
	for _, s := range Surfaces() {
		if s.ID == SurfaceHTTPAPI {
			path = s.Path
		}
	}
	pathMajor, ok := majorFromPath(path)
	if !ok {
		t.Fatalf("http-api path %q is not versioned", path)
	}
	if pathMajor != majorFromVersion(version) {
		t.Fatalf("http-api path major %q != version major of %q", pathMajor, version)
	}
}

func TestMatrixOmitsInternalFields(t *testing.T) {
	matrix := Matrix()
	if len(matrix) != len(Surfaces()) {
		t.Fatalf("matrix length %d != surfaces length %d", len(matrix), len(Surfaces()))
	}
	for _, entry := range matrix {
		if entry.ID == "" || entry.Version == "" {
			t.Fatalf("matrix entry has empty field: %+v", entry)
		}
	}
}

func TestValidateRejectsBrokenManifests(t *testing.T) {
	cases := map[string][]Surface{
		"empty":            {},
		"duplicate id":     {{ID: "http-api", Version: "1.0.0", Path: "/api/v1"}, {ID: "http-api", Version: "1.0.0"}},
		"bad semver":       {{ID: "http-api", Version: "1.x", Path: "/api/v1"}},
		"path major skew":  {{ID: "http-api", Version: "2.0.0", Path: "/api/v1"}},
		"missing http-api": {{ID: "blobstore-spi", Version: "1.0.0"}},
	}
	for name, surfaces := range cases {
		if err := validate(surfaces); err == nil {
			t.Errorf("validate(%s) = nil, want error", name)
		}
	}
}
