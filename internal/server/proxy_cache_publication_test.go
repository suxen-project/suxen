package server

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestDelayedProxy404CannotHideConcurrentPositiveCache(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		default:
			return testHTTPResponse(request, http.StatusOK, "current"), nil
		}
	})})
	createTestRepository(t, fixture, domain.Repository{Name: "mirror-negative", Format: "opaque-index", Type: "proxy", Upstream: "https://upstream.example"})
	done := make(chan *http.Response, 1)
	go func() {
		done <- fixture.request(t, http.MethodGet, "/repository/mirror-negative/index.json", nil, true)
	}()
	<-firstEntered
	positive := fixture.request(t, http.MethodGet, "/repository/mirror-negative/index.json", nil, true)
	_, _ = io.Copy(io.Discard, positive.Body)
	positive.Body.Close()
	if positive.StatusCode != http.StatusOK {
		t.Fatalf("concurrent positive status=%d", positive.StatusCode)
	}
	close(releaseFirst)
	stale := <-done
	_, _ = io.Copy(io.Discard, stale.Body)
	stale.Body.Close()
	if stale.StatusCode != http.StatusNotFound {
		t.Fatalf("delayed response=%d", stale.StatusCode)
	}
	next := fixture.request(t, http.MethodGet, "/repository/mirror-negative/index.json", nil, true)
	defer next.Body.Close()
	body, _ := io.ReadAll(next.Body)
	if next.StatusCode != http.StatusOK {
		t.Fatalf("new positive cache hidden by delayed negative: %d %s (upstream calls=%d)", next.StatusCode, body, calls.Load())
	}
}

func TestLaterProxy404OverridesRecentlyValidatedPositive(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
	positiveEntered := make(chan struct{})
	negativeEntered := make(chan struct{})
	releasePositive := make(chan struct{})
	releaseNegative := make(chan struct{})
	var calls atomic.Int32
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return testHTTPResponse(request, http.StatusOK, "initial"), nil
		case 2:
			close(positiveEntered)
			<-releasePositive
			return testHTTPResponse(request, http.StatusOK, "refreshed"), nil
		case 3:
			close(negativeEntered)
			<-releaseNegative
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		case 4:
			return testHTTPResponse(request, http.StatusOK, "returned-after-expiry"), nil
		default:
			return testHTTPResponse(request, http.StatusInternalServerError, "unexpected upstream call"), nil
		}
	})})
	createTestRepository(t, fixture, domain.Repository{Name: "mirror-later-404", Format: "opaque-index", Type: "proxy", Upstream: "https://upstream.example"})
	read := func() *http.Response {
		return fixture.request(t, http.MethodGet, "/repository/mirror-later-404/index.json", nil, true)
	}
	closeBody := func(response *http.Response) {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	warm := read()
	closeBody(warm)
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm status=%d", warm.StatusCode)
	}
	positiveDone := make(chan *http.Response, 1)
	go func() { positiveDone <- read() }()
	<-positiveEntered
	negativeDone := make(chan *http.Response, 1)
	go func() { negativeDone <- read() }()
	<-negativeEntered
	close(releasePositive)
	positive := <-positiveDone
	closeBody(positive)
	if positive.StatusCode != http.StatusOK {
		t.Fatalf("refreshed status=%d", positive.StatusCode)
	}
	close(releaseNegative)
	negative := <-negativeDone
	closeBody(negative)
	if negative.StatusCode != http.StatusNotFound {
		t.Fatalf("later 404 status=%d", negative.StatusCode)
	}
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = time.Hour })
	latest := read()
	closeBody(latest)
	if latest.StatusCode != http.StatusNotFound || calls.Load() != 3 {
		t.Fatalf("new request status=%d, upstream calls=%d; later 404 must remain authoritative", latest.StatusCode, calls.Load())
	}
	// Advance the negative entry's expiry without sleeping for a minute. A
	// previously refreshed positive must not reappear during its longer TTL.
	repository, err := fixture.Metadata.Repository(context.Background(), "mirror-later-404")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.PutNegativeCacheByRepositoryID(context.Background(), repository.ID, "opaque-cache-index.json", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	afterExpiry := read()
	body, err := io.ReadAll(afterExpiry.Body)
	afterExpiry.Body.Close()
	if err != nil || afterExpiry.StatusCode != http.StatusOK || calls.Load() != 4 {
		t.Fatalf("after negative expiry: status=%d body=%q calls=%d err=%v", afterExpiry.StatusCode, body, calls.Load(), err)
	}
}
