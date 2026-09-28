package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProxyDoesNotCacheIncompleteSuccessResponses(t *testing.T) {
	for _, status := range []int{http.StatusPartialContent, http.StatusNoContent, http.StatusAccepted} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture := newServerFixture(t)
			calls := 0
			fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Header.Get("Range") != "" {
					t.Fatalf("proxy sent unexpected Range request: %q", request.Header.Get("Range"))
				}
				if calls == 1 {
					response := testHTTPResponse(request, status, "half")
					if status == http.StatusPartialContent {
						response.Header.Set("Content-Range", "bytes 0-3/8")
					}
					return response, nil
				}
				return testHTTPResponse(request, http.StatusOK, "complete"), nil
			})})
			createTestRepository(t, fixture, domain.Repository{
				Name: "mirror", Format: "raw", Type: "proxy", Upstream: "https://upstream.example",
			})
			first := fixture.request(t, http.MethodGet, "/repository/mirror/file", nil, true)
			assertStatus(t, first, http.StatusBadGateway)
			first.Body.Close()
			if _, err := fixture.Metadata.Asset(context.Background(), "mirror", "file"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("incomplete response was cached: %v", err)
			}
			second := fixture.request(t, http.MethodGet, "/repository/mirror/file", nil, true)
			assertStatus(t, second, http.StatusOK)
			assertBody(t, second, []byte("complete"))
			if calls != 2 {
				t.Fatalf("upstream calls = %d, want two", calls)
			}
		})
	}
}

func TestProxyCachesCompleteNonAuthoritativeResponse(t *testing.T) {
	fixture := newServerFixture(t)
	calls := 0
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return testHTTPResponse(request, http.StatusNonAuthoritativeInfo, "complete"), nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "mirror", Format: "raw", Type: "proxy", Upstream: "https://upstream.example",
	})
	for range 2 {
		response := fixture.request(t, http.MethodGet, "/repository/mirror/file", nil, true)
		assertStatus(t, response, http.StatusOK)
		assertBody(t, response, []byte("complete"))
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want one cached fetch", calls)
	}
}
