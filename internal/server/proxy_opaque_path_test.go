package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

type opaqueIndexFormat struct{ name string }

type cacheOnlyProxyFormat struct {
	name   string
	vanish func(context.Context, spiformat.StoredAssets, string) error
}

var cacheOnlyFormatCounter atomic.Uint64

func (format cacheOnlyProxyFormat) Name() string { return format.name }
func (format cacheOnlyProxyFormat) ResolveProxyRequest(ctx context.Context, _ spiformat.Repository, path, _ string, stored spiformat.StoredAssets) (spiformat.ResolvedProxyRequest, error) {
	if format.vanish != nil {
		if err := format.vanish(ctx, stored, path); err != nil {
			return spiformat.ResolvedProxyRequest{}, err
		}
	}
	return spiformat.ResolvedProxyRequest{CachePath: path, CacheOnly: true}, nil
}

func (format opaqueIndexFormat) Name() string { return format.name }
func (opaqueIndexFormat) ResolveProxyRequest(_ context.Context, _ spiformat.Repository, path, _ string, _ spiformat.StoredAssets) (spiformat.ResolvedProxyRequest, error) {
	return spiformat.ResolvedProxyRequest{CachePath: "opaque-cache-" + path}, nil
}
func (opaqueIndexFormat) MutableUpstreamPath(_ spiformat.Repository, path string) bool {
	return strings.HasPrefix(path, "index")
}
func (opaqueIndexFormat) RewriteIndex(_ spiformat.Repository, path string, _ []byte, contentType, root string) ([]byte, string, error) {
	if path == "index-error.json" {
		return nil, "", errors.New("index rewrite rejected")
	}
	return []byte(root + "/rewritten-" + path), contentType, nil
}
func (opaqueIndexFormat) ProjectAttributes(asset spiformat.Asset) map[string]any {
	return map[string]any{"path": asset.Path}
}

func init() {
	spiformat.Register(opaqueIndexFormat{name: "opaque-index"})
}

func TestCacheOnlyProxyVanishedEntryNeverFetchesUpstream(t *testing.T) {
	fixture := newServerFixture(t)
	const assetPath = "dl/pkg/1.0.0/download"
	formatName := "proxy-cache-only-test-" + strconv.FormatUint(cacheOnlyFormatCounter.Add(1), 10)
	spiformat.Register(cacheOnlyProxyFormat{name: formatName, vanish: func(ctx context.Context, stored spiformat.StoredAssets, path string) error {
		found := false
		if err := stored.VisitAssetPaths(ctx, path, func(candidate string) (bool, error) {
			found = candidate == path
			return false, nil
		}); err != nil {
			return err
		}
		if !found {
			return errors.New("cached entry not visible to resolver")
		}
		_, err := fixture.Metadata.DeleteAsset(ctx, "cache-only", path)
		return err
	}})
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamCalls++
		return testHTTPResponse(request, http.StatusOK, "wrong upstream body"), nil
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "cache-only", Format: formatName, Type: "proxy", Upstream: "https://upstream.example",
	})
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "cache-only", Path: assetPath, Digest: "sha256:" + strings.Repeat("0", 64), Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodGet, "/repository/cache-only/"+assetPath, nil, true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusNotFound)
	if upstreamCalls != 0 {
		t.Fatalf("cache-only miss fetched upstream %d times", upstreamCalls)
	}
}

func TestOpaqueProxyResolutionPreservesFormatPath(t *testing.T) {
	t.Parallel()
	for _, formatName := range []string{"opaque-index"} {
		t.Run(formatName, func(t *testing.T) {
			fixture := newServerFixture(t)
			fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = time.Hour })
			const upstreamBody = `{"url":"https://upstream.example/artifact"}`
			status := http.StatusOK
			upstreamCalls := 0
			fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				upstreamCalls++
				if request.URL.Path != "/index.json" && request.URL.Path != "/index-error.json" {
					t.Fatalf("upstream path = %q", request.URL.Path)
				}
				response := testHTTPResponse(request, status, upstreamBody)
				response.Header.Set("Content-Type", "application/json")
				return response, nil
			})})
			createTestRepository(t, fixture, domain.Repository{
				Name: "opaque", Format: formatName, Type: "proxy", Upstream: "https://upstream.example",
			})
			createTestRepository(t, fixture, domain.Repository{
				Name: "opaque-group", Format: formatName, Type: "group", Members: []string{"opaque"},
			})
			read := func(path, want string, wantStatus int) {
				t.Helper()
				response := fixture.request(t, http.MethodGet, path, nil, true)
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != wantStatus || (want != "" && string(body) != want) {
					t.Fatalf("GET %s = %d %s; want %d %s", path, response.StatusCode, body, wantStatus, want)
				}
				if wantStatus != http.StatusOK && strings.Contains(string(body), upstreamBody) {
					t.Fatalf("GET %s exposed upstream index on an error", path)
				}
			}
			read("/repository/opaque/index.json", "http://suxen/repository/opaque/rewritten-index.json", http.StatusOK)
			read("/repository/opaque-group/index.json", "http://suxen/repository/opaque-group/rewritten-index.json", http.StatusOK)
			read("/repository/opaque/index.json", "http://suxen/repository/opaque/rewritten-index.json", http.StatusOK)
			if upstreamCalls != 1 {
				t.Fatalf("cache hit fetched upstream %d times", upstreamCalls)
			}
			stored, err := fixture.Metadata.Asset(context.Background(), "opaque", "opaque-cache-index.json")
			if err != nil {
				t.Fatal(err)
			}
			if stored.FormatPath != "index.json" {
				t.Fatalf("stored format path = %q", stored.FormatPath)
			}
			projection := assetattrs.Project(stored, domain.Repository{Name: "opaque", Format: formatName, Type: "proxy"})
			if projection[formatName].(map[string]any)["path"] != "index.json" ||
				projection["sys"].(map[string]any)["path"] != "index.json" {
				t.Fatalf("opaque key changed projected coordinates: %+v", projection)
			}
			reader, _, err := fixture.Handler.blobs.Get(context.Background(), stored.Digest)
			if err != nil {
				t.Fatal(err)
			}
			storedBody, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || string(storedBody) != upstreamBody {
				t.Fatalf("stored bytes = %q, %v", storedBody, err)
			}

			fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
			status = http.StatusNotModified
			read("/repository/opaque/index.json", "http://suxen/repository/opaque/rewritten-index.json", http.StatusOK)
			read("/repository/opaque-group/index.json", "http://suxen/repository/opaque-group/rewritten-index.json", http.StatusOK)
			status = http.StatusInternalServerError
			read("/repository/opaque/index.json", "", http.StatusBadGateway)
			read("/repository/opaque-group/index.json", "", http.StatusBadGateway)
			status = http.StatusOK
			read("/repository/opaque/index-error.json", "", http.StatusInternalServerError)
			read("/repository/opaque-group/index-error.json", "", http.StatusInternalServerError)
		})
	}
}
