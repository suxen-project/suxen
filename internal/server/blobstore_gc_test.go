package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestBlobStoreGCPrunesSingleStore(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	store, err := fixture.Handler.blobStores.Store(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	// A blob with no asset referencing it is exactly what a prune should reclaim.
	payload := []byte("orphan blob")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := store.Put(context.Background(), digest, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}

	preview := decodeGCResult(t, fixture.request(
		t, http.MethodPost, "/api/v1/blob-stores/default/gc?dryRun=true&grace=0", nil, true,
	))
	if preview.BlobStore != "default" || !preview.DryRun {
		t.Fatalf("unexpected preview envelope: %+v", preview)
	}
	if preview.Deleted != 0 || len(preview.WouldDelete) == 0 {
		t.Fatalf("dry run should report but not delete: %+v", preview)
	}

	applied := decodeGCResult(t, fixture.request(
		t, http.MethodPost, "/api/v1/blob-stores/default/gc?dryRun=false&grace=0", nil, true,
	))
	if applied.Deleted != 1 {
		t.Fatalf("apply should delete the orphan: %+v", applied)
	}
	if _, err := store.Head(context.Background(), digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("orphan blob still present: %v", err)
	}
}

func TestBlobStoreGCUnknownStoreNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodPost, "/api/v1/blob-stores/missing/gc", nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
}

func TestBlobStoreGCRequiresAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodPost, "/api/v1/blob-stores/default/gc", nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()
}

func decodeGCResult(t *testing.T, response *http.Response) garbageCollectionResult {
	t.Helper()
	assertStatus(t, response, http.StatusOK)
	var result garbageCollectionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return result
}
