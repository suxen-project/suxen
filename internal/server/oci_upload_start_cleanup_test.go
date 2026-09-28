package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

type canceledCreateUploadStore struct {
	blob.Store
	blob.UploadStore
	cancel    context.CancelFunc
	key       string
	deleteErr error
	createErr error
}

func (store *canceledCreateUploadStore) DeleteUpload(ctx context.Context, key string) error {
	if store.deleteErr != nil {
		return store.deleteErr
	}
	return store.UploadStore.DeleteUpload(ctx, key)
}

func (store *canceledCreateUploadStore) CreateUpload(ctx context.Context, key string) error {
	store.key = key
	if err := store.UploadStore.CreateUpload(ctx, key); err != nil {
		return err
	}
	if store.createErr != nil {
		return store.createErr
	}
	// The backend committed the object, then the request was canceled before
	// the caller observed success. Cleanup must still use a live context.
	store.cancel()
	return context.Canceled
}

func TestConflictingOCIUploadStartRemovesOrphanedObject(t *testing.T) {
	fixture := newServerFixture(t)
	backend := fixture.Handler.blobs.(blob.UploadStore)
	failing := &canceledCreateUploadStore{
		Store: fixture.Handler.blobs, UploadStore: backend,
		createErr: blob.ErrUploadExists,
	}
	resource, err := fixture.Metadata.BlobStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Handler.blobStores.Remember(resource, failing)
	request := httptest.NewRequest(http.MethodPost, "/v2/acme/app/blobs/uploads/", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	fixture.Handler.ServeHTTP(httptest.NewRecorder(), request)
	count, err := fixture.Metadata.CountUploadSessions(context.Background(), "default")
	if err != nil || count != 0 {
		t.Fatalf("conflicting upload left sessions = %d, %v", count, err)
	}
	reader, _, err := backend.OpenUpload(context.Background(), failing.key)
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("conflicting upload left an orphaned staged object: %v", err)
	}
}

func TestFailedOCIUploadStartRetainsSessionUntilPhysicalCleanup(t *testing.T) {
	fixture := newServerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := fixture.Handler.blobs.(blob.UploadStore)
	failing := &canceledCreateUploadStore{
		Store: fixture.Handler.blobs, UploadStore: backend, cancel: cancel,
		deleteErr: errors.New("temporary backend delete failure"),
	}
	resource, err := fixture.Metadata.BlobStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Handler.blobStores.Remember(resource, failing)
	request := httptest.NewRequest(http.MethodPost, "/v2/acme/app/blobs/uploads/", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	fixture.Handler.ServeHTTP(httptest.NewRecorder(), request)

	count, err := fixture.Metadata.CountUploadSessions(context.Background(), "default")
	if err != nil || count != 1 {
		t.Fatalf("failed physical cleanup left recoverable sessions = %d, %v; want one", count, err)
	}
	reader, _, err := backend.OpenUpload(context.Background(), failing.key)
	if err != nil {
		t.Fatalf("staged object was lost while its session remained: %v", err)
	}
	reader.Close()
	failing.deleteErr = nil
	if _, err := fixture.Handler.oci.ReapStaleUploadSessions(context.Background(), false, time.Now().Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	count, err = fixture.Metadata.CountUploadSessions(context.Background(), "default")
	if err != nil || count != 0 {
		t.Fatalf("reaper left sessions = %d, %v; want zero", count, err)
	}
	reader, _, err = backend.OpenUpload(context.Background(), failing.key)
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("reaper left staged object: %v", err)
	}
}

func TestCanceledOCIUploadStartRemovesSessionAndStagedObject(t *testing.T) {
	fixture := newServerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend, ok := fixture.Handler.blobs.(blob.UploadStore)
	if !ok {
		t.Fatal("fixture blob store cannot hold upload sessions")
	}
	failing := &canceledCreateUploadStore{
		Store: fixture.Handler.blobs, UploadStore: backend, cancel: cancel,
	}
	resource, err := fixture.Metadata.BlobStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Handler.blobStores.Remember(resource, failing)

	request := httptest.NewRequest(http.MethodPost, "/v2/acme/app/blobs/uploads/", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)
	if response.Code == http.StatusAccepted {
		t.Fatal("canceled upload start was acknowledged")
	}
	if failing.key == "" {
		t.Fatal("upload backend did not receive a storage key")
	}
	count, err := fixture.Metadata.CountUploadSessions(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("canceled upload left %d quota-counting session rows", count)
	}
	reader, _, err := backend.OpenUpload(context.Background(), failing.key)
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("canceled upload left its staged object: %v", err)
	}
}
