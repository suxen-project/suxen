package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestRepositoryDeletionRetainsStagedOCIUploadsUntilCancelled(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "staging", Format: "oci", Type: "hosted", Writable: true}); err != nil {
		t.Fatal(err)
	}
	start := func() string {
		response := fixture.request(t, http.MethodPost, "/repository/staging/v2/team/application/blobs/uploads/", nil, true)
		assertStatus(t, response, http.StatusAccepted)
		location := response.Header.Get("Location")
		response.Body.Close()
		if location == "" {
			t.Fatal("upload location missing")
		}
		return location
	}
	empty := start()
	partial := start()
	chunk := fixture.request(t, http.MethodPatch, partial, []byte("staged-upload-bytes"), true)
	assertStatus(t, chunk, http.StatusAccepted)
	chunk.Body.Close()
	session, err := fixture.Metadata.UploadSession(ctx, path.Base(partial))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := fixture.Handler.blobStores.Store(ctx, session.BlobStore)
	if err != nil {
		t.Fatal(err)
	}
	uploadStore, ok := physical.(blob.UploadStore)
	if !ok {
		t.Fatal("filesystem store does not expose upload storage")
	}
	reader, size, err := uploadStore.OpenUpload(ctx, session.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || size != int64(len(data)) || string(data) != "staged-upload-bytes" {
		t.Fatalf("staged bytes = %q, %d, %v", data, size, err)
	}

	remove := func() int {
		response := fixture.requestWithBearer(t, http.MethodDelete, "/api/v1/repositories/staging", nil, "", testToken)
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := remove(); status != http.StatusConflict {
		t.Fatalf("delete with two sessions = %d", status)
	}
	if _, err := fixture.Metadata.UploadSession(ctx, session.ID); err != nil {
		t.Fatalf("cleanup ledger lost: %v", err)
	}
	if reader, _, err := uploadStore.OpenUpload(ctx, session.StorageKey); err != nil {
		t.Fatalf("staged object lost: %v", err)
	} else {
		reader.Close()
	}

	stopEmpty := fixture.request(t, http.MethodDelete, empty, nil, true)
	assertStatus(t, stopEmpty, http.StatusNoContent)
	stopEmpty.Body.Close()
	if status := remove(); status != http.StatusConflict {
		t.Fatalf("delete with partial session = %d", status)
	}
	stopPartial := fixture.request(t, http.MethodDelete, partial, nil, true)
	assertStatus(t, stopPartial, http.StatusNoContent)
	stopPartial.Body.Close()
	if _, _, err := uploadStore.OpenUpload(ctx, session.StorageKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancel left staged object: %v", err)
	}
	if status := remove(); status != http.StatusNoContent {
		t.Fatalf("delete after cancellation = %d", status)
	}
}
