package assetattrs

import (
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProjectOverridesReservedCollisions(t *testing.T) {
	now := time.Date(2026, time.August, 6, 10, 30, 0, 0, time.UTC)
	asset := domain.Asset{
		Repository:  "images",
		Path:        "v2/team/service/manifests/v1.2.3",
		Digest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:        42,
		ContentType: "application/vnd.oci.image.manifest.v1+json",
		Kind:        "oci-manifest",
		Reference:   "v1.2.3",
		CreatedAt:   now,
		UpdatedAt:   now,
		Attributes: map[string]any{
			"sys.blobStore":        "flat-forged",
			"classification.label": "flat-forged",
			"sys": map[string]any{
				"blobStore": "forged",
			},
			"classification": map[string]any{
				"label": "release",
			},
			"oci": map[string]any{
				"image":        "forged/image",
				"artifactType": "application/vnd.example.signature",
			},
			"scan": map[string]any{
				"status": "passed",
			},
		},
	}
	repository := domain.Repository{
		Name:      "images",
		Format:    "oci",
		Type:      "hosted",
		BlobStore: "cold-storage",
	}

	projected := Project(asset, repository)
	system := projected["sys"].(map[string]any)
	coordinates := projected["oci"].(map[string]any)
	if system["blobStore"] != "cold-storage" {
		t.Fatalf("sys.blobStore = %v, want repository blob store", system["blobStore"])
	}
	if coordinates["image"] != "team/service" || coordinates["tag"] != "v1.2.3" {
		t.Fatalf("unexpected OCI coordinates: %+v", coordinates)
	}
	if coordinates["artifactType"] != "application/vnd.example.signature" {
		t.Fatalf("OCI artifact type was not exposed: %+v", coordinates)
	}
	if projected["scan"].(map[string]any)["status"] != "passed" {
		t.Fatalf("external namespace was not preserved: %+v", projected)
	}
	if _, found := projected["sys.blobStore"]; found {
		t.Fatalf("flat reserved system alias survived projection: %+v", projected)
	}
	if _, found := projected["classification.label"]; found {
		t.Fatalf("flat reserved classification alias survived projection: %+v", projected)
	}
	if projected["classification"].(map[string]any)["label"] != "release" {
		t.Fatalf("canonical classification was not preserved: %+v", projected)
	}
	if asset.Attributes["sys"].(map[string]any)["blobStore"] != "forged" {
		t.Fatal("projection mutated stored attributes")
	}
}

func TestReservedNamespacesIncludeSystemAndFormatCoordinates(t *testing.T) {
	for _, namespace := range []string{
		"sys",
		"sys.blobStore",
		"oci",
		"oci.image",
		"raw",
		"classification",
		"classification.label",
		"provenance",
	} {
		if !IsReservedNamespace(namespace) {
			t.Errorf("namespace %q is not reserved", namespace)
		}
	}
	if IsReservedNamespace("scan") || IsReservedNamespace("vuln.trivy") {
		t.Fatal("external writer namespaces must remain writable")
	}
}

func TestLookupSupportsDottedWriterNamespacesAndProtectsReservedRoots(t *testing.T) {
	attributes := map[string]any{
		"vuln.trivy":    map[string]any{"severity": "critical"},
		"sys.blobStore": "forged",
		"sys":           map[string]any{"blobStore": "constrained"},
	}
	severity, found := Lookup(attributes, "vuln.trivy.severity")
	if !found || severity != "critical" {
		t.Fatalf("dotted writer namespace lookup = %v, %v", severity, found)
	}
	blobStore, found := Lookup(attributes, "sys.blobStore")
	if !found || blobStore != "constrained" {
		t.Fatalf("reserved lookup accepted flat collision: %v, %v", blobStore, found)
	}
}

func TestOCIImageUsesLastValidRouteSuffix(t *testing.T) {
	tests := []struct {
		path  string
		image string
	}{
		{
			path:  "v2/team/manifests/cache/manifests/latest",
			image: "team/manifests/cache",
		},
		{
			path:  "v2/team/blobs/cache/manifests/latest",
			image: "team/blobs/cache",
		},
		{
			path:  "v2/team/manifests/cache/blobs/sha256:abc",
			image: "team/manifests/cache",
		},
	}
	for _, test := range tests {
		image, found := ociImage(test.path)
		if !found || image != test.image {
			t.Errorf("ociImage(%q) = %q, %v; want %q", test.path, image, found, test.image)
		}
	}
}

func TestSetClassificationLabelsReplacesNamespaceAndKeepsOthers(t *testing.T) {
	attributes := map[string]any{
		"classification": map[string]any{"stale": "old"},
		"provenance":     map[string]any{"status": "verified"},
		"scan":           map[string]any{"status": "passed"},
	}

	updated := SetClassificationLabels(attributes, map[string]string{"stage": "release", "weight": "large"})
	classification := updated["classification"].(map[string]any)
	if classification["stage"] != "release" || classification["weight"] != "large" {
		t.Fatalf("classification labels were not written: %+v", classification)
	}
	if _, present := classification["stale"]; present {
		t.Fatalf("prior classification keys were not replaced: %+v", classification)
	}
	if updated["provenance"] == nil || updated["scan"] == nil {
		t.Fatalf("unrelated namespaces were lost: %+v", updated)
	}

	cleared := SetClassificationLabels(attributes, nil)
	if _, present := cleared["classification"]; present {
		t.Fatalf("empty labels should remove the classification namespace: %+v", cleared)
	}
	if cleared["provenance"] == nil {
		t.Fatalf("clearing classification lost unrelated namespaces: %+v", cleared)
	}
}

func TestKnownAttributePathsCoverProjection(t *testing.T) {
	known := make(map[string]struct{})
	for _, path := range KnownAttributePaths() {
		known[path] = struct{}{}
	}
	now := time.Date(2026, time.August, 6, 10, 30, 0, 0, time.UTC)
	downloaded := now
	cases := []struct {
		asset      domain.Asset
		repository domain.Repository
		roots      []string
	}{
		{
			asset: domain.Asset{
				Repository: "images", Path: "v2/team/service/manifests/v1.2.3",
				Digest:      "sha256:" + "a" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Size:        42,
				ContentType: "application/vnd.oci.image.manifest.v1+json",
				Kind:        "oci-manifest", Reference: "v1.2.3",
				CreatedAt: now, UpdatedAt: now, LastAccessed: &downloaded,
				Attributes: map[string]any{"oci": map[string]any{"artifactType": "application/vnd.example.sig"}},
			},
			repository: domain.Repository{Name: "images", Format: "oci", Type: "hosted", BlobStore: "default"},
			roots:      []string{"sys", "oci"},
		},
		{
			asset: domain.Asset{
				Repository: "raw", Path: "specs/report.json",
				CreatedAt: now, UpdatedAt: now,
			},
			repository: domain.Repository{Name: "raw", Format: "raw", Type: "hosted", BlobStore: "default"},
			roots:      []string{"sys", "raw"},
		},
		{
			asset: domain.Asset{
				Repository: "models", Path: "models/core/0.2.0/SHA256SUMS",
				CreatedAt: now, UpdatedAt: now,
			},
			repository: domain.Repository{
				Name: "models", Format: "raw", Type: "hosted", BlobStore: "default",
				FormatConfig: map[string]any{"components": []any{
					map[string]any{"pattern": `^(?P<name>.+)/(?P<version>[^/]+)/[^/]+$`},
				}},
			},
			roots: []string{"sys", "raw"},
		},
	}
	for _, tc := range cases {
		projected := Project(tc.asset, tc.repository)
		for _, root := range tc.roots {
			namespace, ok := projected[root].(map[string]any)
			if !ok {
				t.Fatalf("projection is missing reserved root %q", root)
			}
			for key := range namespace {
				path := root + "." + key
				if _, listed := known[path]; !listed {
					t.Errorf("projected path %q is not in KnownAttributePaths()", path)
				}
			}
		}
	}
}
