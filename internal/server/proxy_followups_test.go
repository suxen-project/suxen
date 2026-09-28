package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestRawProxyCachesOCIShapedPathWithoutDigestValidation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const upstreamContent = "ordinary raw file"
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testHTTPResponse(request, http.StatusOK, upstreamContent), nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "raw-mirror", Format: "raw", Type: "proxy", Upstream: "https://upstream.example",
	})
	path := "/repository/raw-mirror/v2/acme/app/blobs/" + testDigest([]byte("different file"))
	for range 2 {
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		assertBody(t, response, []byte(upstreamContent))
	}
}

func TestProxyCacheFillWaitsForUploadSlotBeforeReadingBody(t *testing.T) {
	fixture := newServerFixture(t)
	read := make(chan struct{}, 1)
	upstreamReady := make(chan struct{}, 1)
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := testHTTPResponse(request, http.StatusOK, "uncached payload")
		response.Body = &observedProxyBody{ReadCloser: io.NopCloser(strings.NewReader("uncached payload")), read: read}
		upstreamReady <- struct{}{}
		return response, nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "limited-mirror", Format: "raw", Type: "proxy", Upstream: "https://upstream.example",
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
	request := httptest.NewRequest(http.MethodGet, "/repository/limited-mirror/new.txt", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fixture.Handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-upstreamReady:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not receive the upstream response")
	}
	select {
	case <-done:
		t.Fatal("proxy returned before an upload slot became available")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not stop waiting for an upload slot after cancellation")
	}
	if response.Code == http.StatusOK {
		t.Fatal("proxy cached an upstream body while all upload slots were occupied")
	}
	select {
	case <-read:
		t.Fatal("proxy read the upstream body before acquiring an upload slot")
	default:
	}
	if _, err := fixture.Metadata.Asset(context.Background(), "limited-mirror", "new.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("proxy published an asset without an upload slot: %v", err)
	}
	for _, release := range releases {
		release()
	}
	releases = nil
	completed := fixture.request(t, http.MethodGet, "/repository/limited-mirror/new.txt", nil, true)
	assertStatus(t, completed, http.StatusOK)
	assertBody(t, completed, []byte("uncached payload"))
}

type observedProxyBody struct {
	io.ReadCloser
	read chan struct{}
}

func (body *observedProxyBody) Read(buffer []byte) (int, error) {
	select {
	case body.read <- struct{}{}:
	default:
	}
	return body.ReadCloser.Read(buffer)
}

func TestProxyRejectsContentThatDoesNotMatchDigestReference(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upstreamContent := []byte("content with a different digest")
	requestedDigest := testDigest([]byte("expected content"))
	upstreamDigest := testDigest(upstreamContent)

	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return testHTTPResponse(request, http.StatusOK, string(upstreamContent)), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "digest-mirror",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	})

	requestPath := "/repository/digest-mirror/v2/acme/app/blobs/" + requestedDigest
	response := fixture.request(t, http.MethodGet, requestPath, nil, true)
	assertStatus(t, response, http.StatusBadGateway)
	response.Body.Close()

	if _, err := fixture.Handler.blobs.Head(context.Background(), requestedDigest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("requested digest was persisted after a mismatch: %v", err)
	}
	if _, err := fixture.Handler.blobs.Head(context.Background(), upstreamDigest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("mismatched upstream content was persisted: %v", err)
	}
	if _, err := fixture.Metadata.Asset(
		context.Background(),
		"digest-mirror",
		"v2/acme/app/blobs/"+requestedDigest,
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("proxy metadata was persisted after a mismatch: %v", err)
	}
}

func TestProxyRevalidatesTaggedManifestAndPreservesUpstreamRepresentation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	// A very short TTL makes the third (cache-hit) request depend on CI runner
	// load: under the race detector the gap after revalidation can exceed the
	// TTL and trigger an extra upstream call. Leave headroom instead.
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 2 * time.Second })
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	const mediaType = "application/vnd.docker.distribution.manifest.v2+json"
	const accept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"

	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			if request.Header.Get("Accept") != accept {
				t.Fatalf("upstream Accept header = %q, want %q", request.Header.Get("Accept"), accept)
			}
			if upstreamCalls == 1 {
				response := testHTTPResponse(request, http.StatusOK, string(manifest))
				response.Header.Set("Content-Type", mediaType)
				return response, nil
			}
			if request.Header.Get("If-None-Match") != content.QuoteETag(testDigest(manifest)) {
				t.Fatalf("upstream If-None-Match = %q", request.Header.Get("If-None-Match"))
			}
			return testHTTPResponse(request, http.StatusNotModified, ""), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "tag-mirror",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	})

	requestPath := "/repository/tag-mirror/v2/acme/app/manifests/latest"
	first := requestWithAccept(t, fixture, requestPath, accept)
	assertStatus(t, first, http.StatusOK)
	if first.Header.Get("Content-Type") != mediaType {
		t.Fatalf("first response Content-Type = %q, want %q", first.Header.Get("Content-Type"), mediaType)
	}
	assertBody(t, first, manifest)

	time.Sleep(fixture.Handler.cfg.ProxyManifestTTL + 250*time.Millisecond)
	second := requestWithAccept(t, fixture, requestPath, accept)
	assertStatus(t, second, http.StatusOK)
	if second.Header.Get("Content-Type") != mediaType {
		t.Fatalf("revalidated response Content-Type = %q, want %q", second.Header.Get("Content-Type"), mediaType)
	}
	assertBody(t, second, manifest)

	third := requestWithAccept(t, fixture, requestPath, accept)
	assertStatus(t, third, http.StatusOK)
	assertBody(t, third, manifest)
	if upstreamCalls != 2 {
		t.Fatalf("upstream called %d times, want initial fetch and one revalidation", upstreamCalls)
	}
}

func TestProxyForwardsRepeatedAcceptLikeCombinedAccept(t *testing.T) {
	fixture := newServerFixture(t)
	const want = "text/html,application/json"
	var observed []string
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		observed = append(observed, request.Header.Get("Accept"))
		return testHTTPResponse(request, http.StatusOK, "asset"), nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "accept-mirror", Format: "raw", Type: "proxy", Upstream: "https://upstream.example",
	})
	for _, test := range []struct {
		name   string
		values []string
	}{
		{"combined", []string{want}},
		{"repeated", []string{"text/html", "application/json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/repository/accept-mirror/"+test.name, nil)
			request.Header.Set("Authorization", "Bearer "+testToken)
			for _, value := range test.values {
				request.Header.Add("Accept", value)
			}
			recorder := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || recorder.Body.String() != "asset" {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
	if len(observed) != 2 || observed[0] != want || observed[1] != want {
		t.Fatalf("forwarded Accept values = %q, want two %q values", observed, want)
	}
}

func TestProxyRevalidationDoesNotUseLocalFetchTimeAsUpstreamModificationTime(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
	oldManifest := []byte(`{"schemaVersion":2,"annotations":{"revision":"old"}}`)
	newManifest := []byte(`{"schemaVersion":2,"annotations":{"revision":"new"}}`)
	oldModified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	newModified := oldModified.Add(30 * time.Minute)
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			if upstreamCalls == 1 {
				response := testHTTPResponse(request, http.StatusOK, string(oldManifest))
				response.Header.Set("Last-Modified", oldModified.Format(http.TimeFormat))
				return response, nil
			}
			// This upstream has only a Last-Modified validator. Its new
			// representation has an mtime older than the proxy's first fetch.
			if header := request.Header.Get("If-Modified-Since"); header != "" {
				conditionalTime, err := http.ParseTime(header)
				if err != nil {
					t.Fatalf("invalid If-Modified-Since %q: %v", header, err)
				}
				if !newModified.After(conditionalTime) {
					return testHTTPResponse(request, http.StatusNotModified, ""), nil
				}
			}
			response := testHTTPResponse(request, http.StatusOK, string(newManifest))
			response.Header.Set("Last-Modified", newModified.Format(http.TimeFormat))
			return response, nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name: "mtime-mirror", Format: "oci", Type: "proxy", Upstream: "https://registry.example",
	})
	path := "/repository/mtime-mirror/v2/acme/app/manifests/latest"
	first := fixture.request(t, http.MethodGet, path, nil, true)
	assertStatus(t, first, http.StatusOK)
	assertBody(t, first, oldManifest)
	second := fixture.request(t, http.MethodGet, path, nil, true)
	assertStatus(t, second, http.StatusOK)
	assertBody(t, second, newManifest)
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstreamCalls)
	}
}

func TestProxyDoesNotExpireDigestPinnedManifest(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	manifestDigest := testDigest(manifest)
	upstreamCalls := 0

	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			response := testHTTPResponse(request, http.StatusOK, string(manifest))
			response.Header.Set("Content-Type", ociManifestMediaTypeForTest)
			return response, nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "pinned-mirror",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	})

	requestPath := "/repository/pinned-mirror/v2/acme/app/manifests/" + manifestDigest
	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(t, http.MethodGet, requestPath, nil, true)
		assertStatus(t, response, http.StatusOK)
		assertBody(t, response, manifest)
	}
	if upstreamCalls != 1 {
		t.Fatalf("digest-pinned manifest fetched %d times, want once", upstreamCalls)
	}
}

func TestGroupContinuesAfterMemberMissesAndErrors(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	content := []byte("hosted fallback")
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/fallback.bin",
		content,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Host {
			case "missing.example":
				return testHTTPResponse(request, http.StatusNotFound, "not found"), nil
			case "broken.example":
				return testHTTPResponse(request, http.StatusInternalServerError, "broken"), nil
			default:
				t.Fatalf("unexpected upstream %q", request.URL.Host)
				return nil, nil
			}
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "missing-member",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://missing.example",
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "broken-member",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://broken.example",
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:    "fallback-group",
		Format:  "raw",
		Type:    "group",
		Members: []string{"missing-member", "broken-member", "raw"},
	})

	fallback := fixture.request(
		t,
		http.MethodGet,
		"/repository/fallback-group/fallback.bin",
		nil,
		true,
	)
	assertStatus(t, fallback, http.StatusOK)
	assertBody(t, fallback, content)

	createTestRepository(t, fixture, domain.Repository{
		Name:    "failed-group",
		Format:  "raw",
		Type:    "group",
		Members: []string{"missing-member", "broken-member"},
	})
	failed := fixture.request(
		t,
		http.MethodGet,
		"/repository/failed-group/fallback.bin",
		nil,
		true,
	)
	assertStatus(t, failed, http.StatusBadGateway)
	failed.Body.Close()
}

func createTestRepository(t *testing.T, fixture *serverFixture, repository domain.Repository) {
	t.Helper()
	if err := fixture.Metadata.CreateRepository(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
}

func requestWithAccept(
	t *testing.T,
	fixture *serverFixture,
	requestPath string,
	accept string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, requestPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
