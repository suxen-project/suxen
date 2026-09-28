package ocimodel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseRouteUsesOnlyAnchoredOperationSegments(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
		want        Route
	}{
		{
			name:        "blob name containing manifests",
			requestPath: "team/manifests/cache/blobs/sha256:abc",
			want: Route{
				Kind:      BlobRoute,
				ImageName: "team/manifests/cache",
				Value:     "sha256:abc",
			},
		},
		{
			name:        "manifest name containing blobs",
			requestPath: "team/blobs/cache/manifests/latest",
			want: Route{
				Kind:      ManifestRoute,
				ImageName: "team/blobs/cache",
				Value:     "latest",
			},
		},
		{
			name:        "tags name containing tags",
			requestPath: "team/tags/cache/tags/list",
			want: Route{
				Kind:      TagsRoute,
				ImageName: "team/tags/cache",
			},
		},
		{
			name:        "referrers name containing referrers",
			requestPath: "team/referrers/cache/referrers/sha256:def",
			want: Route{
				Kind:      ReferrersRoute,
				ImageName: "team/referrers/cache",
				Value:     "sha256:def",
			},
		},
		{
			name:        "upload name containing blobs",
			requestPath: "team/blobs/cache/blobs/uploads/0123456789abcdef",
			want: Route{
				Kind:      UploadRoute,
				ImageName: "team/blobs/cache",
				Value:     "0123456789abcdef",
			},
		},
		{
			name:        "upload start with conventional trailing slash",
			requestPath: "/team/blobs/cache/blobs/uploads/",
			want: Route{
				Kind:      UploadRoute,
				ImageName: "team/blobs/cache",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ParseRoute(test.requestPath)
			if !ok {
				t.Fatalf("ParseRoute(%q) did not recognize the route", test.requestPath)
			}
			if got != test.want {
				t.Fatalf("ParseRoute(%q) = %+v, want %+v", test.requestPath, got, test.want)
			}
		})
	}
}

func TestParseRouteRejectsMalformedPaths(t *testing.T) {
	paths := []string{
		"",
		"/",
		"manifests/latest",
		"team/manifests",
		"team/manifests/latest/",
		"team//manifests/latest",
		"team/blobs",
		"team/blobs/uploads/id/extra",
		"team/tags",
		"team/tags/latest",
		"team/referrers",
		"team/unknown/latest",
		"_catalog",
		"v2/_catalog",
	}

	for _, requestPath := range paths {
		t.Run(requestPath, func(t *testing.T) {
			if route, ok := ParseRoute(requestPath); ok {
				t.Fatalf("ParseRoute(%q) unexpectedly returned %+v", requestPath, route)
			}
		})
	}
}

func TestParseAssetPathRequiresV2Prefix(t *testing.T) {
	if route, ok := ParseAssetPath("team/cache/manifests/latest"); ok {
		t.Fatalf("ParseAssetPath without v2 prefix returned %+v", route)
	}
	got, ok := ParseAssetPath("v2/team/cache/manifests/latest")
	if !ok {
		t.Fatal("ParseAssetPath did not recognize a valid asset path")
	}
	want := Route{Kind: ManifestRoute, ImageName: "team/cache", Value: "latest"}
	if got != want {
		t.Fatalf("ParseAssetPath = %+v, want %+v", got, want)
	}
}

func TestRouteValidateDistributionGrammar(t *testing.T) {
	for _, test := range []struct {
		path  string
		valid bool
	}{
		{path: "team/api_v2/manifests/Release-1.0", valid: true},
		{path: "team__cache/manifests/_latest", valid: true},
		{path: "team/api/manifests/sha256:" + strings.Repeat("a", 64), valid: true},
		{path: "team/api/manifests/bad tag"},
		{path: "team/api/manifests/" + strings.Repeat("a", 129)},
		{path: "team/api/manifests/sha256:short"},
		{path: "Team/api/manifests/latest"},
		{path: "team/-api/manifests/latest"},
		{path: "team/api_/manifests/latest"},
		{path: strings.Repeat("a", 256) + "/manifests/latest"},
		{path: strings.Repeat("a", 255) + "/manifests/latest", valid: true},
	} {
		route, ok := ParseRoute(test.path)
		if !ok {
			t.Fatalf("ParseRoute(%q) failed before validation", test.path)
		}
		if got := route.Validate() == nil; got != test.valid {
			t.Errorf("Validate(%q) valid = %t, want %t", test.path, got, test.valid)
		}
	}
}

func TestValidateHostedManifestDescriptors(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "empty index", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, valid: true},
		{name: "valid config", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digest + `","size":0},"layers":[]}`, valid: true},
		{name: "missing config", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`},
		{name: "missing config digest", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":0},"layers":[]}`},
		{name: "missing config size", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digest + `"},"layers":[]}`},
		{name: "missing layer media type", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digest + `","size":0},"layers":[{"digest":"` + digest + `","size":0}]}`},
		{name: "invalid config media type", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"not a media type","digest":"` + digest + `","size":0},"layers":[]}`},
		{name: "negative index descriptor size", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":-1}]}`},
		{name: "missing subject digest", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","size":1}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var manifest ManifestEnvelope
			if err := json.Unmarshal([]byte(test.body), &manifest); err != nil {
				t.Fatal(err)
			}
			if got := ValidateHostedManifest(manifest) == nil; got != test.valid {
				t.Errorf("ValidateHostedManifest valid = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestManifestDependenciesDeduplicatesAndSorts(t *testing.T) {
	manifest := ManifestEnvelope{
		Config: &Descriptor{Digest: "sha256:config"},
		Layers: []Descriptor{
			{Digest: "sha256:b"},
			{Digest: "sha256:a"},
			{Digest: "sha256:b"},
			{Digest: ""},
		},
		Manifests: []Descriptor{{Digest: "sha256:a"}},
		Subject:   &Descriptor{Digest: "sha256:subject"},
	}
	got := ManifestDependencies(manifest)
	want := []string{"sha256:a", "sha256:b", "sha256:config", "sha256:subject"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ManifestDependencies = %v, want %v", got, want)
	}
}
