package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// opaqueKeyFormat models a proxy format whose cache identity is deliberately
// distinct from the public request path: it stores every read under a
// "cache/<path>" key while the format's mutable-path policy and index rewriter
// only recognize the public path. It reproduces the host contract for a format
// whose ResolveProxyRequest returns a cache key that is not the request path.
type opaqueKeyFormat struct{}

const (
	opaqueUpstreamMarker = "UPSTREAM_MARKER"
	opaqueMutablePath    = "index.json"
	opaqueRejectPath     = "forbidden.json"
)

func (opaqueKeyFormat) Name() string { return "proxy-opaque" }

func (opaqueKeyFormat) ResolveProxyRequest(
	_ context.Context,
	_ spiformat.Repository,
	assetPath string,
	_ string,
	_ spiformat.StoredAssets,
) (spiformat.ResolvedProxyRequest, error) {
	if assetPath == opaqueRejectPath {
		return spiformat.ResolvedProxyRequest{}, &spiformat.PolicyViolation{
			Code:    "proxy_opaque_forbidden",
			Message: "path is not served",
		}
	}
	// An opaque cache key that is never equal to the public request path.
	return spiformat.ResolvedProxyRequest{CachePath: "cache/" + assetPath}, nil
}

func (opaqueKeyFormat) MutableUpstreamPath(_ spiformat.Repository, assetPath string) bool {
	return assetPath == opaqueMutablePath
}

func (opaqueKeyFormat) RewriteIndex(
	_ spiformat.Repository,
	_ string,
	body []byte,
	contentType string,
	repositoryURL string,
) ([]byte, string, error) {
	return []byte(strings.ReplaceAll(string(body), opaqueUpstreamMarker, repositoryURL)), contentType, nil
}

func init() { spiformat.Register(opaqueKeyFormat{}) }

// opaqueIndexBody is the upstream document carrying a URL the rewriter must
// replace so it points back at this repository.
var opaqueIndexBody = []byte(`{"file":"` + opaqueUpstreamMarker + `/files/pkg.tgz"}`)

// A format that caches an index under an opaque key still gets that index
// rewritten on the original mutable path, and the row is persisted and cached
// under the opaque key, never the public path.
func TestProxyOpaqueCacheKeyRewritesOnOriginalPath(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = time.Hour })

	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			return testHTTPResponse(request, http.StatusOK, string(opaqueIndexBody)), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "opaque-mirror",
		Format:   "proxy-opaque",
		Type:     "proxy",
		Upstream: "https://upstream.example",
	})

	requestPath := "/repository/opaque-mirror/" + opaqueMutablePath

	first := fixture.request(t, http.MethodGet, requestPath, nil, true)
	assertStatus(t, first, http.StatusOK)
	firstBody := readBody(t, first)
	if strings.Contains(firstBody, opaqueUpstreamMarker) {
		t.Fatalf("first read was not rewritten: %q", firstBody)
	}
	if !strings.Contains(firstBody, "/repository/opaque-mirror") {
		t.Fatalf("first read does not point back at the repository: %q", firstBody)
	}

	// Second read is a cache hit within the TTL: still rewritten, no re-fetch.
	second := fixture.request(t, http.MethodGet, requestPath, nil, true)
	assertStatus(t, second, http.StatusOK)
	secondBody := readBody(t, second)
	if secondBody != firstBody {
		t.Fatalf("cache hit body = %q, want %q", secondBody, firstBody)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream called %d times, want 1 (second read served from cache)", upstreamCalls)
	}

	// Persistence and cache identity use the opaque key, not the public path.
	if _, err := fixture.Metadata.Asset(context.Background(), "opaque-mirror", "cache/"+opaqueMutablePath); err != nil {
		t.Fatalf("row not persisted under the opaque cache key: %v", err)
	}
	if _, err := fixture.Metadata.Asset(context.Background(), "opaque-mirror", opaqueMutablePath); !isNotFound(err) {
		t.Fatalf("row leaked under the public path: %v", err)
	}
}

// The opaque cache key drives revalidation too: after the TTL the host
// revalidates under the opaque key and still rewrites the original path.
func TestProxyOpaqueCacheKeyRevalidates(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = time.Second })

	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			if upstreamCalls == 1 {
				return testHTTPResponse(request, http.StatusOK, string(opaqueIndexBody)), nil
			}
			if request.Header.Get("If-None-Match") == "" {
				t.Fatal("revalidation did not send If-None-Match")
			}
			return testHTTPResponse(request, http.StatusNotModified, ""), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "opaque-reval",
		Format:   "proxy-opaque",
		Type:     "proxy",
		Upstream: "https://upstream.example",
	})

	requestPath := "/repository/opaque-reval/" + opaqueMutablePath
	first := fixture.request(t, http.MethodGet, requestPath, nil, true)
	assertStatus(t, first, http.StatusOK)
	first.Body.Close()

	time.Sleep(fixture.Handler.cfg.ProxyManifestTTL + 250*time.Millisecond)
	second := fixture.request(t, http.MethodGet, requestPath, nil, true)
	assertStatus(t, second, http.StatusOK)
	body := readBody(t, second)
	if strings.Contains(body, opaqueUpstreamMarker) {
		t.Fatalf("revalidated read was not rewritten: %q", body)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream called %d times, want initial fetch and one revalidation", upstreamCalls)
	}
}

// A 404 on the opaque key is negatively cached under that key: the second read
// is served from the negative cache without a second upstream call.
func TestProxyOpaqueCacheKeyNegativeCache(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			return testHTTPResponse(request, http.StatusNotFound, "missing"), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "opaque-miss",
		Format:   "proxy-opaque",
		Type:     "proxy",
		Upstream: "https://upstream.example",
	})

	requestPath := "/repository/opaque-miss/absent.json"
	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(t, http.MethodGet, requestPath, nil, true)
		assertStatus(t, response, http.StatusNotFound)
		response.Body.Close()
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream called %d times, want 1 (second read served from negative cache)", upstreamCalls)
	}
}

// A resolver rejection is surfaced to the client before cache or upstream.
func TestProxyOpaqueResolverRejection(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			return testHTTPResponse(request, http.StatusOK, "unexpected"), nil
		}),
	})
	createTestRepository(t, fixture, domain.Repository{
		Name:     "opaque-reject",
		Format:   "proxy-opaque",
		Type:     "proxy",
		Upstream: "https://upstream.example",
	})

	response := fixture.request(t, http.MethodGet, "/repository/opaque-reject/"+opaqueRejectPath, nil, true)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
	if upstreamCalls != 0 {
		t.Fatalf("upstream called %d times, want 0 (rejected before fetch)", upstreamCalls)
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	var builder strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buffer)
		builder.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	return builder.String()
}
