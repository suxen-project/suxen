package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// migratingAssetRead forces migration after the request takes its asset
// snapshot, before the data plane opens the corresponding blob.
type migratingAssetRead struct {
	store.Store
	once    sync.Once
	migrate func()
}

type migratingAssetView struct {
	store.RepositoryView
	owner *migratingAssetRead
}

func (metadata *migratingAssetRead) ForRepository(repository domain.Repository) store.RepositoryView {
	return migratingAssetView{metadata.Store.ForRepository(repository), metadata}
}

func (view migratingAssetView) Asset(ctx context.Context, path string) (domain.Asset, error) {
	asset, err := view.RepositoryView.Asset(ctx, path)
	if err == nil {
		view.owner.once.Do(view.owner.migrate)
	}
	return asset, err
}

func TestDownloadOverlappingBlobStoreMigration(t *testing.T) {
	h := newDrainTestHandler(t)
	t.Cleanup(func() { _ = h.Close() })
	for _, setup := range []struct{ path, body string }{
		{"/api/v1/blob-stores", `{"name":"secondary","driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"}}`},
		{"/api/v1/repositories", `{"name":"movable","format":"raw","type":"hosted","blobStore":"secondary"}`},
	} {
		response := blobStoreRequest(t, h, http.MethodPost, setup.path, setup.body)
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", setup.path, response.Code, response.Body.String())
		}
	}
	if response := ociConcurrencyRequest(h, http.MethodPut, "/repository/movable/file", "payload", ""); response.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("start drain: %d %s", response.Code, response.Body.String())
	}
	metadata := &migratingAssetRead{Store: h.metadata}
	metadata.migrate = func() {
		if err := h.runBlobStoreMigration(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.content.SetMetadata(metadata)
	response = ociConcurrencyRequest(h, http.MethodGet, "/repository/movable/file", "", "")
	if response.Code != http.StatusOK || response.Body.String() != "payload" {
		t.Fatalf("download overlapping migration = %d %s; want 200 payload", response.Code, response.Body.String())
	}
}

func TestStoredAssetReadDoesNotFollowReplacement(t *testing.T) {
	h := newDrainTestHandler(t)
	t.Cleanup(func() { _ = h.Close() })
	response := ociConcurrencyRequest(h, http.MethodPut, "/repository/raw/file", "first", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload first: %d %s", response.Code, response.Body.String())
	}
	ctx := context.Background()
	old, err := h.metadata.Asset(ctx, "raw", "file")
	if err != nil {
		t.Fatal(err)
	}
	response = ociConcurrencyRequest(h, http.MethodPut, "/repository/raw/file", "second", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload second: %d %s", response.Code, response.Body.String())
	}
	current, err := h.metadata.Asset(ctx, "raw", "file")
	if err != nil {
		t.Fatal(err)
	}
	if current.Digest == old.Digest {
		t.Fatal("replacement kept the original digest")
	}
	blobStore, err := h.blobStores.Store(ctx, old.BlobStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := blobStore.Delete(ctx, old.Digest); err != nil {
		t.Fatal(err)
	}
	reader, _, err := h.content.OpenStoredAsset(ctx, old)
	if reader != nil {
		_ = reader.Close()
		t.Fatal("opened a deleted generation")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("open deleted generation = %v; want not found", err)
	}
}
