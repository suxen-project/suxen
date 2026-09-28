package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

type admissionWireFormat struct {
	entered chan struct{}
	read    chan struct{}
}

var testAdmissionWireFormat = &admissionWireFormat{}

func init() { spiformat.Register(testAdmissionWireFormat) }

func (*admissionWireFormat) Name() string { return "wire-admission" }

func (*admissionWireFormat) WireAction(_ spiformat.Repository, method, path string, _ url.Values) (string, bool) {
	if path != "fill" {
		return "", false
	}
	if method == http.MethodPost {
		return "write", true
	}
	return "read", true
}

func (format *admissionWireFormat) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	_ spiformat.Repository,
	_ string,
	tools spiformat.WireTools,
) {
	if format.entered != nil {
		format.entered <- struct{}{}
	}
	body := &observedProxyBody{
		ReadCloser: io.NopCloser(strings.NewReader("cached payload")),
		read:       format.read,
	}
	if _, err := tools.StoreAsset(r.Context(), "cached", "application/octet-stream", body); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TestReadWireCacheFillWaitsForUploadSlot(t *testing.T) {
	fixture := newServerFixture(t)
	testAdmissionWireFormat.entered = make(chan struct{}, 1)
	testAdmissionWireFormat.read = make(chan struct{}, 1)
	defer func() {
		testAdmissionWireFormat.entered = nil
		testAdmissionWireFormat.read = nil
	}()
	createTestRepository(t, fixture, domain.Repository{
		Name: "wire-proxy", Format: testAdmissionWireFormat.Name(), Type: "proxy", Upstream: "https://upstream.example",
	})

	var releases []func()
	for range 4 {
		release, err := fixture.Handler.content.AcquireUpload(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/repository/wire-proxy/fill", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fixture.Handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-testAdmissionWireFormat.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wire format was not dispatched")
	}
	select {
	case <-done:
		t.Fatal("wire cache fill returned before an upload slot became available")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wire cache fill did not stop waiting after cancellation")
	}
	select {
	case <-testAdmissionWireFormat.read:
		t.Fatal("wire cache fill read content before acquiring a slot")
	default:
	}
	if _, err := fixture.Metadata.Asset(context.Background(), "wire-proxy", "cached"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wire cache fill published without a slot: %v", err)
	}
	for _, release := range releases {
		release()
	}
	releases = nil
	completed := fixture.request(t, http.MethodGet, "/repository/wire-proxy/fill", nil, true)
	assertStatus(t, completed, http.StatusOK)
	completed.Body.Close()
	if _, err := fixture.Metadata.Asset(context.Background(), "wire-proxy", "cached"); err != nil {
		t.Fatalf("wire cache fill did not publish with an available slot: %v", err)
	}
}

func TestWriteWireStoreAssetUsesItsExistingUploadSlot(t *testing.T) {
	fixture := newServerFixture(t)
	createTestRepository(t, fixture, domain.Repository{
		Name: "wire-hosted", Format: testAdmissionWireFormat.Name(), Type: "hosted",
	})
	var releases []func()
	for range 3 {
		release, err := fixture.Handler.content.AcquireUpload(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/repository/wire-hosted/fill", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("write action with one available slot returned HTTP %d: %s", response.Code, response.Body.String())
	}
}
