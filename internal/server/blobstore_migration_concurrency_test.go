package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type migrationSnapshot struct {
	store.Store
	captured, proceed chan struct{}
	blocked           atomic.Bool
}

func (s *migrationSnapshot) BlobStores(ctx context.Context) ([]domain.BlobStore, error) {
	resources, err := s.Store.BlobStores(ctx)
	if s.blocked.CompareAndSwap(false, true) {
		close(s.captured)
		<-s.proceed
	}
	return resources, err
}
func TestMigrationHonorsCancelledDrain(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	ctx := context.Background()
	resp := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`)
	if resp.Code != 200 {
		t.Fatalf("drain %d %s", resp.Code, resp.Body.String())
	}
	barrier := &migrationSnapshot{Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{})}
	h.metadata = barrier
	h.content.SetMetadata(barrier)
	done := make(chan error, 1)
	go func() { done <- h.runBlobStoreMigration(ctx) }()
	<-barrier.captured
	resp = blobStoreRequest(t, h, http.MethodDelete, "/api/v1/blob-stores/secondary/drain", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("cancel drain = %d: %s", resp.Code, resp.Body.String())
	}
	close(barrier.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := h.metadata.BlobStore(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.metadata.Repository(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.BlobStoreStateActive || current.DrainTarget != "" || repo.BlobStore != "secondary" {
		t.Fatalf("migration executed a drain already cancelled successfully")
	}
}

func TestMigrationUsesReplacementDrainTarget(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	ctx := context.Background()
	t.Setenv("SUXEN_PASS5_THIRD_STORE", "tracking://"+t.TempDir())
	created := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores", `{"name":"third","driver":"tracking","configurationRef":{"env":"SUXEN_PASS5_THIRD_STORE"}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create third store = %d: %s", created.Code, created.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`); response.Code != http.StatusOK {
		t.Fatalf("initial drain = %d: %s", response.Code, response.Body.String())
	}
	barrier := &migrationSnapshot{Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{})}
	h.metadata = barrier
	h.content.SetMetadata(barrier)
	done := make(chan error, 1)
	go func() { done <- h.runBlobStoreMigration(ctx) }()
	<-barrier.captured
	if response := blobStoreRequest(t, h, http.MethodDelete, "/api/v1/blob-stores/secondary/drain", ""); response.Code != http.StatusOK {
		close(barrier.proceed)
		t.Fatalf("cancel drain = %d: %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"third"}`); response.Code != http.StatusOK {
		close(barrier.proceed)
		t.Fatalf("replacement drain = %d: %s", response.Code, response.Body.String())
	}
	close(barrier.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	resource, err := h.metadata.BlobStore(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.metadata.Repository(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	if resource.State != domain.BlobStoreStateDrained || resource.DrainTarget != "third" || repo.BlobStore != "third" {
		t.Fatalf("migration used stale target: store=%+v repository=%+v", resource, repo)
	}
}

func TestStoreEditPreservesPendingUpload(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != 202 {
		t.Fatal(start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	patch := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2")
	if patch.Code != 202 {
		t.Fatal(patch.Code, patch.Body.String())
	}
	rebind := blobStoreRequest(t, h, http.MethodPut, "/api/v1/repositories/images", `{"format":"oci","type":"hosted","blobStore":"default"}`)
	if rebind.Code != 200 {
		t.Fatal(rebind.Code, rebind.Body.String())
	}
	before := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if before.Code != 204 {
		t.Fatalf("upload unavailable before configuration edit: %d %s", before.Code, before.Body.String())
	}
	t.Setenv("SUXEN_PASS5_REPLACEMENT_STORE", "tracking://"+t.TempDir())
	edit := blobStoreRequest(t, h, http.MethodPut, "/api/v1/blob-stores/secondary", `{"driver":"tracking","configurationRef":{"env":"SUXEN_PASS5_REPLACEMENT_STORE"}}`)
	if edit.Code != http.StatusConflict {
		t.Fatalf("configuration edit with upload = %d: %s", edit.Code, edit.Body.String())
	}
	after := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if after.Code != http.StatusNoContent {
		t.Fatalf("upload status after rejected edit = %d: %s", after.Code, after.Body.String())
	}
	// Attribute-only edits do not change the physical store and remain allowed.
	attributes := blobStoreRequest(t, h, http.MethodPut, "/api/v1/blob-stores/secondary", `{"driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"},"attributes":{"note":"safe"}}`)
	if attributes.Code != http.StatusOK {
		t.Fatalf("attribute-only edit = %d: %s", attributes.Code, attributes.Body.String())
	}
	digest := testDigest([]byte("abc"))
	if finalized := ociConcurrencyRequest(h, http.MethodPut, location+"?digest="+digest, "", ""); finalized.Code != http.StatusCreated {
		t.Fatalf("finalize upload = %d: %s", finalized.Code, finalized.Body.String())
	}
	// Asset homes may remain on the old physical store after a repository
	// rebind (for example, a mounted blob). Such a reference also blocks edits.
	secondary, err := h.blobStores.Store(context.Background(), "secondary")
	if err != nil {
		t.Fatal(err)
	}
	physicalDigest := testDigest([]byte("physical"))
	if _, err := secondary.Put(context.Background(), physicalDigest, strings.NewReader("physical")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "images", Path: "v2/legacy/blobs/" + physicalDigest,
		Digest: physicalDigest, Size: int64(len("physical")), Kind: "oci-blob", BlobStore: "secondary",
	}); err != nil {
		t.Fatal(err)
	}
	if edit = blobStoreRequest(t, h, http.MethodPut, "/api/v1/blob-stores/secondary", `{"driver":"tracking","configurationRef":{"env":"SUXEN_PASS5_REPLACEMENT_STORE"}}`); edit.Code != http.StatusConflict {
		t.Fatalf("configuration edit with physical asset = %d: %s", edit.Code, edit.Body.String())
	}
	if drain := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`); drain.Code != http.StatusOK {
		t.Fatalf("drain after finalization = %d: %s", drain.Code, drain.Body.String())
	}
	if err := h.runBlobStoreMigration(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A store's definition is fixed for its lifetime: even once it is drained
	// empty, its configuration cannot be edited in place. Changing the backend
	// means deleting the store and creating a new one.
	if edit = blobStoreRequest(t, h, http.MethodPut, "/api/v1/blob-stores/secondary", `{"driver":"tracking","configurationRef":{"env":"SUXEN_PASS5_REPLACEMENT_STORE"}}`); edit.Code != http.StatusConflict {
		t.Fatalf("configuration edit after migration = %d: %s", edit.Code, edit.Body.String())
	}
}
