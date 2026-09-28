package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func decodeVerifyResult(t *testing.T, response *http.Response) blobStoreVerifyResult {
	t.Helper()
	defer response.Body.Close()
	var result blobStoreVerifyResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode verify result: %v", err)
	}
	return result
}

func TestVerifyReportsDanglingAndOrphanedBlobs(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()

	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/keep.bin",
		[]byte("kept payload"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	blobStore, err := fixture.Handler.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	// An orphan: physically present, referenced by no asset.
	orphan := []byte("orphan payload")
	orphanSum := sha256.Sum256(orphan)
	orphanDigest := "sha256:" + hex.EncodeToString(orphanSum[:])
	if _, err := blobStore.Put(ctx, orphanDigest, bytes.NewReader(orphan)); err != nil {
		t.Fatalf("stage orphan blob: %v", err)
	}

	// A dangling reference: the kept asset's blob removed from under it.
	blobs, err := collectBlobs(ctx, blobStore)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range blobs {
		if info.Digest == orphanDigest {
			continue
		}
		if err := blobStore.Delete(ctx, info.Digest); err != nil {
			t.Fatalf("delete kept blob: %v", err)
		}
	}

	response := fixture.request(t, http.MethodPost, "/api/v1/verify", nil, true)
	assertStatus(t, response, http.StatusOK)
	result := decodeVerifyResult(t, response)

	if result.DanglingTotal != 1 {
		t.Errorf("danglingTotal = %d, want 1", result.DanglingTotal)
	}
	if result.OrphanedTotal != 1 {
		t.Errorf("orphanedTotal = %d, want 1", result.OrphanedTotal)
	}
	if result.MismatchedTotal != 0 {
		t.Errorf("mismatchedTotal = %d, want 0", result.MismatchedTotal)
	}
	if len(result.Dangling) != 1 {
		t.Fatalf("dangling findings = %d, want 1", len(result.Dangling))
	}
	if result.Rehash {
		t.Error("rehash reported true without the query parameter")
	}

	metrics := fixture.request(t, http.MethodGet, "/metrics", nil, true)
	assertStatus(t, metrics, http.StatusOK)
	families := parseMetricResponse(t, metrics)
	assertMetricValue(t, families, "suxen_blob_verify_findings", map[string]string{
		"kind": "dangling",
	}, 1)
	assertMetricValue(t, families, "suxen_blob_verify_findings", map[string]string{
		"kind": "orphaned",
	}, 1)
}

func TestVerifyRehashPassesOnIntactStore(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/intact.bin",
		[]byte("intact payload"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	response := fixture.request(t, http.MethodPost, "/api/v1/verify?rehash=true", nil, true)
	assertStatus(t, response, http.StatusOK)
	result := decodeVerifyResult(t, response)

	if !result.Rehash {
		t.Error("rehash not reported despite the query parameter")
	}
	if result.Rehashed < 1 {
		t.Errorf("rehashed = %d, want at least 1", result.Rehashed)
	}
	if result.MismatchedTotal != 0 {
		t.Errorf("mismatchedTotal = %d, want 0", result.MismatchedTotal)
	}
	if result.DanglingTotal != 0 {
		t.Errorf("danglingTotal = %d, want 0", result.DanglingTotal)
	}
}

func TestVerifyIgnoresUncachedOCIProxyDependenciesButFindsMissingPublishedBlob(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	configContent := []byte("config bytes")
	configDigest := testDigest(configContent)
	layerDigest := testDigest([]byte("layer not cached"))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":16}]}`, configDigest, len(configContent), layerDigest)
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/manifests/") {
			response := testHTTPResponse(request, http.StatusOK, manifest)
			response.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			return response, nil
		}
		if strings.HasSuffix(request.URL.Path, "/blobs/"+configDigest) {
			return testHTTPResponse(request, http.StatusOK, string(configContent)), nil
		}
		return testHTTPResponse(request, http.StatusNotFound, ""), nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "verify-oci-proxy", Format: "oci", Type: "proxy", Upstream: "https://registry.example",
	})
	manifestResponse := fixture.request(t, http.MethodGet, "/repository/verify-oci-proxy/v2/acme/app/manifests/latest", nil, true)
	assertStatus(t, manifestResponse, http.StatusOK)
	manifestResponse.Body.Close()

	ctx := context.Background()
	retained, err := fixture.Metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := retained[configDigest]; !found {
		t.Fatal("GC no longer retains the uncached config dependency")
	}
	if _, found := retained[layerDigest]; !found {
		t.Fatal("GC no longer retains the uncached layer dependency")
	}
	before, err := fixture.Handler.verifyBlobStores(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if before.DanglingTotal != 0 {
		t.Fatalf("uncached OCI dependencies reported as dangling: %+v", before)
	}

	blobResponse := fixture.request(t, http.MethodGet, "/repository/verify-oci-proxy/v2/acme/app/blobs/"+configDigest, nil, true)
	assertStatus(t, blobResponse, http.StatusOK)
	blobResponse.Body.Close()
	physical, err := fixture.Handler.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := physical.Delete(ctx, configDigest); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.Handler.verifyBlobStores(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if after.DanglingTotal != 1 || len(after.Dangling) != 1 || after.Dangling[0].Digest != configDigest {
		t.Fatalf("missing published config was not the sole dangling blob: %+v", after)
	}
}

func TestVerifyUnknownBlobStoreIsNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodPost, "/api/v1/verify?blobStore=missing", nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
}

type verifyReferenceSnapshot struct {
	store.Store
	after func()
}

func (s *verifyReferenceSnapshot) PublishedReferencedDigests(ctx context.Context, name string) (map[string]struct{}, error) {
	references, err := s.Store.PublishedReferencedDigests(ctx, name)
	if s.after != nil {
		after := s.after
		s.after = nil
		after()
	}
	return references, err
}

type verifyConcurrentStore struct {
	blob.Store
	beforeGet func()
	omitWalk  bool
}

func (s *verifyConcurrentStore) Get(ctx context.Context, digest string) (io.ReadCloser, domain.BlobInfo, error) {
	if s.beforeGet != nil {
		before := s.beforeGet
		s.beforeGet = nil
		before()
	}
	return s.Store.Get(ctx, digest)
}

func (s *verifyConcurrentStore) Walk(ctx context.Context, yield func(domain.BlobInfo) error) error {
	if s.omitWalk {
		return nil
	}
	return s.Store.Walk(ctx, yield)
}

func TestVerifyRechecksConcurrentStorageChanges(t *testing.T) {
	for _, scenario := range []string{"delete-before-walk", "delete-before-rehash", "listing-misses-present-blob"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			fixture := newServerFixture(t)
			response := fixture.request(t, http.MethodPut, "/repository/raw/file.txt", []byte("payload"), true)
			assertStatus(t, response, http.StatusCreated)
			response.Body.Close()

			deleteAndCollect := func() {
				response := fixture.request(t, http.MethodDelete, "/repository/raw/file.txt", nil, true)
				assertStatus(t, response, http.StatusNoContent)
				response.Body.Close()
				result, err := fixture.Handler.collectGarbage(context.Background(), false, 0, time.Now().Add(time.Second), "default")
				if err != nil {
					t.Fatal(err)
				}
				if result.Deleted != 1 {
					t.Fatalf("GC deleted %d blobs, want 1", result.Deleted)
				}
			}
			if scenario == "delete-before-walk" {
				fixture.Handler.metadata = &verifyReferenceSnapshot{Store: fixture.Metadata, after: deleteAndCollect}
			} else {
				backing, err := blob.NewFS(filepath.Join(fixture.Handler.cfg.DataDir, "blobs"))
				if err != nil {
					t.Fatal(err)
				}
				concurrent := &verifyConcurrentStore{Store: backing, omitWalk: scenario == "listing-misses-present-blob"}
				if scenario == "delete-before-rehash" {
					concurrent.beforeGet = deleteAndCollect
				}
				resource, err := fixture.Metadata.BlobStore(context.Background(), "default")
				if err != nil {
					t.Fatal(err)
				}
				fixture.Handler.blobStores.Remember(resource, concurrent)
			}
			result, err := fixture.Handler.verifyBlobStores(context.Background(), "default", scenario == "delete-before-rehash")
			if err != nil {
				t.Fatal(err)
			}
			if result.DanglingTotal != 0 || len(result.Dangling) != 0 {
				t.Fatalf("healthy store reported dangling blobs: %+v", result)
			}
		})
	}
}
