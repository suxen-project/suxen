package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

type missingMigrationTarget struct{ blob.Store }

func (s missingMigrationTarget) Put(context.Context, string, io.Reader) (domain.BlobInfo, error) {
	return domain.BlobInfo{}, fmt.Errorf("target staging file unavailable: %w", os.ErrNotExist)
}

func TestMigrationMustNotRebindAfterMissingTargetCopyFailure(t *testing.T) {
	h := newDrainTestHandler(t)
	for _, c := range []struct{ path, body string }{
		{"/api/v1/blob-stores", `{"name":"secondary","driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"}}`},
		{"/api/v1/repositories", `{"name":"movable","format":"raw","type":"hosted","blobStore":"secondary"}`},
	} {
		w := blobStoreRequest(t, h, http.MethodPost, c.path, c.body)
		if w.Code != http.StatusCreated {
			t.Fatalf("setup %d: %s", w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/repository/movable/file", strings.NewReader("keep me"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx := context.Background()
	resource, err := h.metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	h.blobStores.Remember(resource, missingMigrationTarget{Store: h.blobs})
	w = blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target":"default"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	err = h.runBlobStoreMigration(ctx)
	asset, lookupErr := h.metadata.Asset(ctx, "movable", "file")
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	req = httptest.NewRequest(http.MethodGet, "/repository/movable/file", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !errors.Is(err, os.ErrNotExist) || asset.BlobStore != "secondary" || w.Code != http.StatusOK || w.Body.String() != "keep me" {
		t.Fatalf("failed copy: migration error=%v, asset store=%s, download=%d; want error, secondary, 200", err, asset.BlobStore, w.Code)
	}

	// Restoring the target permits the same drain to resume safely.
	h.blobStores.Remember(resource, h.blobs)
	if err := h.runBlobStoreMigration(ctx); err != nil {
		t.Fatal(err)
	}
	asset, err = h.metadata.Asset(ctx, "movable", "file")
	if err != nil || asset.BlobStore != "default" {
		t.Fatalf("retry asset = %+v, error = %v", asset, err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "keep me" {
		t.Fatalf("download after retry = %d, %q", w.Code, w.Body.String())
	}
}
