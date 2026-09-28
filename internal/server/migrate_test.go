package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestBlobStoreMigrationMovesDataAndDrains(t *testing.T) {
	handler := newDrainTestHandler(t)
	ctx := context.Background()

	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary", "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create store = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "movable", "format": "raw", "type": "hosted", "blobStore": "secondary"
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create repo = %d: %s", got.Code, got.Body.String())
	}

	payload := []byte("payload to migrate")
	upload := httptest.NewRequest(http.MethodPut, "/repository/movable/artifact.bin", bytes.NewReader(payload))
	upload.Header.Set("Authorization", "Bearer "+testToken)
	uploadRec := httptest.NewRecorder()
	handler.ServeHTTP(uploadRec, upload)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", uploadRec.Code, uploadRec.Body.String())
	}

	secondaryStore, err := handler.blobStores.Store(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if before, err := collectBlobs(ctx, secondaryStore); err != nil || len(before) != 1 {
		t.Fatalf("secondary blobs before migration = %d (err %v), want 1", len(before), err)
	}

	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "default"}`); got.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", got.Code, got.Body.String())
	}

	if err := handler.runBlobStoreMigration(ctx); err != nil {
		t.Fatalf("migration: %v", err)
	}

	drained, err := handler.metadata.BlobStore(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if drained.State != domain.BlobStoreStateDrained {
		t.Fatalf("secondary state = %q, want drained", drained.State)
	}
	if after, err := collectBlobs(ctx, secondaryStore); err != nil || len(after) != 0 {
		t.Fatalf("secondary blobs after migration = %d (err %v), want 0", len(after), err)
	}

	repository, err := handler.metadata.Repository(ctx, "movable")
	if err != nil {
		t.Fatal(err)
	}
	if repository.BlobStore != "default" {
		t.Fatalf("repository blob store = %q, want default", repository.BlobStore)
	}

	download := httptest.NewRequest(http.MethodGet, "/repository/movable/artifact.bin", nil)
	download.Header.Set("Authorization", "Bearer "+testToken)
	downloadRec := httptest.NewRecorder()
	handler.ServeHTTP(downloadRec, download)
	if downloadRec.Code != http.StatusOK || downloadRec.Body.String() != string(payload) {
		t.Fatalf("download after migration = %d body=%q", downloadRec.Code, downloadRec.Body.String())
	}

	// A drained store references no repository and is safe to remove.
	if got := blobStoreRequest(t, handler, http.MethodDelete, "/api/v1/blob-stores/secondary", ""); got.Code != http.StatusNoContent {
		t.Fatalf("delete drained store = %d: %s", got.Code, got.Body.String())
	}
}

func TestBlobStoreMigrationNoDrainingStoresIsNoop(t *testing.T) {
	handler := newDrainTestHandler(t)
	if err := handler.runBlobStoreMigration(context.Background()); err != nil {
		t.Fatalf("migration with no draining stores: %v", err)
	}
}

func TestOCIManifestPublishedDuringDrainRetainsSourceBlob(t *testing.T) {
	handler := newDrainTestHandler(t)
	ctx := context.Background()

	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary", "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create store = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "images", "format": "oci", "type": "hosted", "blobStore": "secondary"
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create repository = %d: %s", got.Code, got.Body.String())
	}
	request := func(method string, path string, body []byte, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	payload := []byte("source-store OCI configuration")
	digest := testDigest(payload)
	blobPath := "/repository/images/v2/app/blobs/" + digest
	upload := request(
		http.MethodPost,
		"/repository/images/v2/app/blobs/uploads/?digest="+digest,
		payload,
		"application/octet-stream",
	)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", upload.Code, upload.Body.String())
	}
	if got := blobStoreRequest(
		t,
		handler,
		http.MethodPost,
		"/api/v1/blob-stores/secondary/drain",
		`{"target":"default"}`,
	); got.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", got.Code, got.Body.String())
	}

	manifestPath := "/repository/images/v2/app/manifests/latest"
	manifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`,
		digest,
		len(payload),
	))
	barrier := &migrationGCBarrier{
		Store:              handler.metadata,
		captureStore:       "secondary",
		referencesCaptured: make(chan struct{}),
		continueGC:         make(chan struct{}),
		leaseContended:     make(chan struct{}),
	}
	handler.metadata = barrier
	handler.content.SetMetadata(barrier)
	type gcOutcome struct {
		result garbageCollectionResult
		err    error
	}
	gcDone := make(chan gcOutcome, 1)
	go func() {
		result, collectErr := handler.collectGarbage(
			ctx,
			false,
			24*time.Hour,
			time.Now().Add(48*time.Hour),
			"secondary",
		)
		gcDone <- gcOutcome{result: result, err: collectErr}
	}()
	select {
	case <-barrier.referencesCaptured:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not capture source references")
	}

	publishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		publishDone <- request(
			http.MethodPut,
			manifestPath,
			manifest,
			"application/vnd.oci.image.manifest.v1+json",
		)
	}()
	select {
	case <-barrier.leaseContended:
		// Publication is waiting on the drain target while GC holds both stores.
	case <-time.After(5 * time.Second):
		t.Fatal("manifest publication did not contend for the drain-target lease")
	}
	close(barrier.continueGC)
	gc := <-gcDone
	if gc.err != nil {
		t.Fatal(gc.err)
	}
	if gc.result.Deleted != 0 {
		t.Fatalf("source GC deleted from a draining store: %+v", gc.result)
	}
	published := <-publishDone
	if published.Code != http.StatusCreated {
		t.Fatalf("publish manifest = %d: %s", published.Code, published.Body.String())
	}
	manifestDigest := published.Header().Get("Docker-Content-Digest")

	references, err := handler.metadata.ReferencedDigests(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := references[digest]; !found {
		t.Fatalf("source-store references = %v, want %s", references, digest)
	}
	for _, path := range []string{blobPath, manifestPath} {
		response := request(http.MethodGet, path, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
		}
	}

	deleted := request(
		http.MethodDelete,
		"/repository/images/v2/app/manifests/"+manifestDigest,
		nil,
		"",
	)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete manifest = %d: %s", deleted.Code, deleted.Body.String())
	}
	cleared := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/blob-stores/secondary/drain",
		"",
	)
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear drain = %d: %s", cleared.Code, cleared.Body.String())
	}
	secondGC, err := handler.collectGarbage(
		ctx,
		false,
		24*time.Hour,
		time.Now().Add(48*time.Hour),
		"secondary",
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondGC.Deleted != 1 {
		t.Fatalf("source GC result after manifest deletion = %+v, want one deletion", secondGC)
	}
	if _, err := handler.metadata.Asset(
		ctx,
		"images",
		"v2/app/blobs/"+digest,
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("orphan OCI blob metadata error = %v, want not found", err)
	}
	response := request(http.MethodGet, blobPath, nil, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET reclaimed blob = %d: %s", response.Code, response.Body.String())
	}
}

type migrationGCBarrier struct {
	store.Store
	captureStore       string
	referencesCaptured chan struct{}
	continueGC         chan struct{}
	leaseContended     chan struct{}
	captureOnce        sync.Once
	contentionOnce     sync.Once
}

func (metadata *migrationGCBarrier) ReferencedDigests(
	ctx context.Context,
	name string,
) (map[string]struct{}, error) {
	references, err := metadata.Store.ReferencedDigests(ctx, name)
	captureStore := metadata.captureStore
	if captureStore == "" {
		captureStore = "default"
	}
	if name == captureStore {
		metadata.captureOnce.Do(func() { close(metadata.referencesCaptured) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-metadata.continueGC:
		}
	}
	return references, err
}

func (metadata *migrationGCBarrier) AcquireLease(
	ctx context.Context,
	name string,
	holder string,
	now time.Time,
	expiresAt time.Time,
) (bool, error) {
	acquired, err := metadata.Store.AcquireLease(ctx, name, holder, now, expiresAt)
	if err == nil && !acquired {
		metadata.contentionOnce.Do(func() { close(metadata.leaseContended) })
	}
	return acquired, err
}

func TestBlobStoreMigrationExcludesTargetGarbageCollection(t *testing.T) {
	handler := newDrainTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary", "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create store = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "movable", "format": "raw", "type": "hosted", "blobStore": "secondary"
    }`); got.Code != http.StatusCreated {
		t.Fatalf("create repository = %d: %s", got.Code, got.Body.String())
	}

	payload := []byte("payload at risk")
	upload := httptest.NewRequest(http.MethodPut, "/repository/movable/artifact.bin", bytes.NewReader(payload))
	upload.Header.Set("Authorization", "Bearer "+testToken)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	asset, err := handler.metadata.Asset(ctx, "movable", "artifact.bin")
	if err != nil {
		t.Fatal(err)
	}
	target, err := handler.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	// Seed an old, currently unreferenced duplicate on the drain target. GC may
	// legitimately classify it for deletion before migration starts.
	if _, err := target.Put(ctx, asset.Digest, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if got := blobStoreRequest(
		t,
		handler,
		http.MethodPost,
		"/api/v1/blob-stores/secondary/drain",
		`{"target":"default"}`,
	); got.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", got.Code, got.Body.String())
	}

	barrier := &migrationGCBarrier{
		Store:              handler.metadata,
		referencesCaptured: make(chan struct{}),
		continueGC:         make(chan struct{}),
		leaseContended:     make(chan struct{}),
	}
	handler.metadata = barrier
	handler.content.SetMetadata(barrier)
	gcDone := make(chan error, 1)
	go func() {
		_, collectErr := handler.collectGarbage(
			ctx,
			false,
			0,
			time.Now().Add(time.Second),
			"default",
		)
		gcDone <- collectErr
	}()
	select {
	case <-barrier.referencesCaptured:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	migrationDone := make(chan error, 1)
	go func() { migrationDone <- handler.runBlobStoreMigration(ctx) }()
	select {
	case <-barrier.leaseContended:
		// Migration reached the target lease and is excluded by GC.
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(barrier.continueGC)
	if err := <-gcDone; err != nil {
		t.Fatalf("garbage collection: %v", err)
	}
	if err := <-migrationDone; err != nil {
		t.Fatalf("migration: %v", err)
	}

	moved, err := handler.metadata.Asset(ctx, "movable", "artifact.bin")
	if err != nil {
		t.Fatal(err)
	}
	if moved.BlobStore != "default" {
		t.Fatalf("asset blob store = %q, want default", moved.BlobStore)
	}
	source, err := handler.blobStores.Store(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Head(ctx, asset.Digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("source blob error = %v, want not found after migration", err)
	}
	if _, err := target.Head(ctx, asset.Digest); err != nil {
		t.Fatalf("live target blob was deleted: %v", err)
	}
}
