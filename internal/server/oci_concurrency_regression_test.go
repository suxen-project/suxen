package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/store"
)

func ociConcurrencyRequest(h *Server, method, path, body, cr string) *httptest.ResponseRecorder {
	return ociConcurrencyRequestWithToken(h, method, path, body, cr, testToken)
}

func ociConcurrencyRequestWithToken(
	h *Server,
	method string,
	path string,
	body string,
	contentRange string,
	token string,
) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if contentRange != "" {
		r.Header.Set("Content-Range", contentRange)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func newOCIConcurrencyServer(t *testing.T, secondary bool) *Server {
	h := newDrainTestHandler(t)
	binding := "default"
	if secondary {
		binding = "secondary"
		if w := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores", `{"name":"secondary","driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"}}`); w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := blobStoreRequest(t, h, http.MethodPost, "/api/v1/repositories", fmt.Sprintf(`{"name":"images","format":"oci","type":"hosted","blobStore":%q}`, binding)); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return h
}

type uploadSessionReadBarrier struct {
	store.Store
	captured, proceed chan struct{}
	calls             atomic.Int32
}

func (m *uploadSessionReadBarrier) UploadSession(ctx context.Context, id string) (store.UploadSession, error) {
	session, err := m.Store.UploadSession(ctx, id)
	if m.calls.Add(1) == 1 {
		close(m.captured)
		<-m.proceed
	}
	return session, err
}
func TestConcurrentSameRangeAppendsOnce(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != 202 {
		t.Fatal(start.Code)
	}
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	barrier := &uploadSessionReadBarrier{Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{})}
	h.content.SetMetadata(barrier)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2") }()
	<-barrier.captured
	second := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2")
	close(barrier.proceed)
	first := <-done
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	t.Logf("same-range PATCH results %d / %d; stored Range=%s", first.Code, second.Code, status.Header().Get("Range"))
	if first.Code != http.StatusRequestedRangeNotSatisfiable ||
		second.Code != http.StatusAccepted {
		t.Fatalf("same-range PATCH results = %d / %d, want 416 / 202", first.Code, second.Code)
	}
	if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-2" {
		t.Fatalf("upload status = %d Range %q, want 204 and 0-2", status.Code, status.Header().Get("Range"))
	}
	backing, err := h.blobStores.Store(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	reader, size, err := backing.(blob.UploadStore).OpenUpload(
		context.Background(), ociUploadStorageKeyForTest("images", id),
	)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || size != 3 || string(stored) != "abc" {
		t.Fatalf("stored upload = %q size %d error %v", stored, size, err)
	}
}

type gcPublicationBarrier struct {
	store.Store
	captured, proceed, waiting chan struct{}
	armed, signaled            atomic.Bool
}

func (m *gcPublicationBarrier) ReferencedDigests(ctx context.Context, name string) (map[string]struct{}, error) {
	refs, err := m.Store.ReferencedDigests(ctx, name)
	m.armed.Store(true)
	close(m.captured)
	<-m.proceed
	return refs, err
}
func (m *gcPublicationBarrier) AcquireLease(ctx context.Context, name, holder string, now, expires time.Time) (bool, error) {
	if m.armed.Load() && m.signaled.CompareAndSwap(false, true) {
		close(m.waiting)
	}
	return m.Store.AcquireLease(ctx, name, holder, now, expires)
}
func TestGCAndManifestPublicationAreAtomic(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	assertGCAndManifestPublicationAreAtomic(t, h, "images", "default", testToken)
}

func assertGCAndManifestPublicationAreAtomic(
	t *testing.T,
	h *Server,
	repositoryName string,
	storeName string,
	token string,
) {
	t.Helper()
	ctx := context.Background()
	payload := "old config"
	sum := sha256.Sum256([]byte(payload))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	request := func(method string, path string, body string) *httptest.ResponseRecorder {
		return ociConcurrencyRequestWithToken(h, method, path, body, "", token)
	}
	prefix := "/repository/" + repositoryName + "/v2/app"
	if w := request(http.MethodPost, prefix+"/blobs/uploads/?digest="+digest, payload); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	barrier := &gcPublicationBarrier{Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{}), waiting: make(chan struct{})}
	h.metadata = barrier
	h.content.SetMetadata(barrier)
	done := make(chan error, 1)
	go func() {
		_, err := h.collectGarbage(ctx, false, 24*time.Hour, time.Now().Add(48*time.Hour), storeName)
		done <- err
	}()
	<-barrier.captured
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":%d},"layers":[]}`, digest, len(payload))
	pub := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		pub <- request(http.MethodPut, prefix+"/manifests/latest", manifest)
	}()
	<-barrier.waiting
	close(barrier.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	w := <-pub
	blobResponse := request(http.MethodGet, prefix+"/blobs/"+digest, "")
	tag := request(http.MethodGet, prefix+"/manifests/latest", "")
	t.Logf("manifest PUT=%d manifest GET=%d config GET=%d", w.Code, tag.Code, blobResponse.Code)
	if w.Code != http.StatusBadRequest || tag.Code != http.StatusNotFound ||
		blobResponse.Code != http.StatusNotFound {
		t.Fatalf(
			"GC-winning race = manifest PUT %d, manifest GET %d, blob GET %d; want 400/404/404",
			w.Code, tag.Code, blobResponse.Code,
		)
	}
}

type staleUploadSelectionBarrier struct {
	store.Store
	captured, proceed chan struct{}
}

func (m *staleUploadSelectionBarrier) StaleUploadSessions(ctx context.Context, name string, before, now time.Time) ([]store.UploadSession, error) {
	sessions, err := m.Store.StaleUploadSessions(ctx, name, before, now)
	close(m.captured)
	<-m.proceed
	return sessions, err
}
func TestUploadReaperPreservesResumedSession(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	ctx := context.Background()
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != 202 {
		t.Fatal(start.Code)
	}
	id, location := start.Header().Get("Docker-Upload-UUID"), start.Header().Get("Location")
	session, err := h.metadata.UploadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	limits := store.UploadSessionLimits{MaxStagedBytes: 16 << 20, MaxPrincipalStagedBytes: 16 << 20, MaxPrincipalSessions: 4}
	if _, err := h.metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity, "aging-fixture", now, now.Add(time.Minute), 0, true, 16<<20, limits); err != nil {
		t.Fatal(err)
	}
	if err := h.metadata.CommitUploadSessionAppend(ctx, id, "aging-fixture", 0, now.Add(-7*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	barrier := &staleUploadSelectionBarrier{Store: h.metadata, captured: make(chan struct{}), proceed: make(chan struct{})}
	h.content.SetMetadata(barrier)
	done := make(chan error, 1)
	go func() { _, err := h.oci.ReapStaleUploadSessions(ctx, false, now); done <- err }()
	<-barrier.captured
	appended := ociConcurrencyRequest(h, http.MethodPatch, location, "fresh data", "0-9")
	if appended.Code != 202 {
		close(barrier.proceed)
		t.Fatal(appended.Code, appended.Body.String())
	}
	close(barrier.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	t.Logf("resumed upload PATCH=%d subsequent GET=%d", appended.Code, status.Code)
	if status.Code != 204 {
		t.Fatal("reaper deleted freshly resumed upload after successful append")
	}
	if status.Header().Get("Range") != "0-9" {
		t.Fatalf("resumed upload Range = %q, want 0-9", status.Header().Get("Range"))
	}
}
