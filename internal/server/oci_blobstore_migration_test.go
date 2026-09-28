package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type uploadBindingReadBarrier struct {
	store.Store
	captured, proceed chan struct{}
	blocked           atomic.Bool
}

func (barrier *uploadBindingReadBarrier) WriteBlobStore(ctx context.Context, name string) (string, error) {
	resolved, err := barrier.Store.WriteBlobStore(ctx, name)
	if name == "secondary" && barrier.blocked.CompareAndSwap(false, true) {
		close(barrier.captured)
		<-barrier.proceed
	}
	return resolved, err
}

func TestUploadStartRetriesBindingThatMovedBeforeLease(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	ctx := context.Background()
	barrier := &uploadBindingReadBarrier{
		Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{}),
	}
	h.metadata = barrier
	h.content.SetMetadata(barrier)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	}()
	<-barrier.captured
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`); response.Code != http.StatusOK {
		close(barrier.proceed)
		t.Fatalf("drain = %d: %s", response.Code, response.Body.String())
	}
	if err := h.runBlobStoreMigration(ctx); err != nil {
		close(barrier.proceed)
		t.Fatal(err)
	}
	close(barrier.proceed)
	start := <-done
	if start.Code != http.StatusAccepted {
		t.Fatalf("start after migration = %d: %s", start.Code, start.Body.String())
	}
	session, err := h.metadata.UploadSession(ctx, start.Header().Get("Docker-Upload-UUID"))
	if err != nil || session.BlobStore != "default" {
		t.Fatalf("accepted upload stored in %q, %v", session.BlobStore, err)
	}
}

func TestMountUsesPhysicalStoreThroughDrainAndMigration(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	ctx := context.Background()
	payload := "mounted blob"
	sum := sha256.Sum256([]byte(payload))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	base := "/repository/images/v2/"
	if response := ociConcurrencyRequest(h, http.MethodPost, base+"source/blobs/uploads/?digest="+digest, payload, ""); response.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", response.Code, response.Body.String())
	}
	mount := func(image string) {
		t.Helper()
		response := ociConcurrencyRequest(h, http.MethodPost, base+image+"/blobs/uploads/?mount="+digest+"&from=source", "", "")
		if response.Code != http.StatusCreated {
			t.Fatalf("mount %s = %d: %s", image, response.Code, response.Body.String())
		}
		read := ociConcurrencyRequest(h, http.MethodGet, base+image+"/blobs/"+digest, "", "")
		if read.Code != http.StatusOK || read.Body.String() != payload {
			t.Fatalf("mounted %s unreadable: %d %s", image, read.Code, read.Body.String())
		}
	}
	mount("active")
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`); response.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", response.Code, response.Body.String())
	}
	mount("draining")
	if err := h.runBlobStoreMigration(ctx); err != nil {
		t.Fatal(err)
	}
	mount("migrated")
	if _, err := h.collectGarbage(ctx, false, 24*time.Hour, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"active", "draining", "migrated"} {
		read := ociConcurrencyRequest(h, http.MethodGet, base+image+"/blobs/"+digest, "", "")
		if read.Code != http.StatusOK || read.Body.String() != payload {
			t.Fatalf("%s after migration/GC = %d %s", image, read.Code, read.Body.String())
		}
	}
}

func TestMigrationWaitsForResumableUpload(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	ctx := context.Background()
	base := "/repository/images/v2/app/blobs/uploads/"
	start := ociConcurrencyRequest(h, http.MethodPost, base, "", "")
	if start.Code != http.StatusAccepted {
		t.Fatalf("start = %d: %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	if response := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2"); response.Code != http.StatusAccepted {
		t.Fatalf("append = %d: %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`); response.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", response.Code, response.Body.String())
	}
	if err := h.runBlobStoreMigration(ctx); err != nil {
		t.Fatal(err)
	}
	resource, err := h.metadata.BlobStore(ctx, "secondary")
	if err != nil || resource.State != domain.BlobStoreStateDraining {
		t.Fatalf("source retired with active upload: %q, %v", resource.State, err)
	}
	progress, err := h.migrateStore(ctx, resource)
	if err != nil || progress.PendingUploads != 1 || progress.Drained {
		t.Fatalf("pending migration = %+v, %v", progress, err)
	}
	if response := ociConcurrencyRequestWithToken(h, http.MethodGet, location, "", "", "invalid-token"); response.Code == http.StatusNoContent {
		t.Fatal("another principal accessed the upload")
	}
	if response := ociConcurrencyRequest(h, http.MethodPatch, location, "def", "3-5"); response.Code != http.StatusAccepted {
		t.Fatalf("resume = %d: %s", response.Code, response.Body.String())
	}
	sum := sha256.Sum256([]byte("abcdef"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if response := ociConcurrencyRequest(h, http.MethodPut, location+"?digest="+digest, "", ""); response.Code != http.StatusCreated {
		t.Fatalf("finalize = %d: %s", response.Code, response.Body.String())
	}
	if err := h.runBlobStoreMigration(ctx); err != nil {
		t.Fatal(err)
	}
	resource, err = h.metadata.BlobStore(ctx, "secondary")
	if err != nil || resource.State != domain.BlobStoreStateDrained {
		t.Fatalf("source not retired after completion: %q, %v", resource.State, err)
	}
	read := ociConcurrencyRequest(h, http.MethodGet, "/repository/images/v2/app/blobs/"+digest, "", "")
	if read.Code != http.StatusOK || read.Body.String() != "abcdef" {
		t.Fatalf("completed blob unreadable: %d %s", read.Code, read.Body.String())
	}
}
