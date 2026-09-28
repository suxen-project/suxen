package pypi_test

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pypi "github.com/suxen-project/suxen/plugins/format/pypi"
	"github.com/suxen-project/suxen/spi/format"
)

func testSHA256(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

func TestPyPIProxyRejectsAdvertisedDigestMismatchThenAcceptsRepair(t *testing.T) {
	for _, representation := range []string{"html", "json"} {
		for _, companion := range []bool{false, true} {
			name := representation
			if companion {
				name += "-metadata"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, time.Hour)
				var repaired atomic.Bool
				var downloads atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/simple/widget/", "/simple/widget":
						if representation == "json" {
							w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
							fmt.Fprintf(w, `{"name":"widget","files":[{"filename":"widget-1.0.whl","url":"/widget-1.0.whl","hashes":{"sha256":"%s"},"core-metadata":{"sha256":"%s"}}]}`, testSHA256("wheel-correct"), testSHA256("metadata-correct"))
						} else {
							fmt.Fprintf(w, `<a href="/widget-1.0.whl#sha256=%s" data-core-metadata="sha256=%s">widget-1.0.whl</a>`, testSHA256("wheel-correct"), testSHA256("metadata-correct"))
						}
					case "/widget-1.0.whl", "/widget-1.0.whl.metadata":
						downloads.Add(1)
						if !repaired.Load() {
							fmt.Fprint(w, "corrupt")
						} else if strings.HasSuffix(r.URL.Path, ".metadata") {
							fmt.Fprint(w, "metadata-correct")
						} else {
							fmt.Fprint(w, "wheel-correct")
						}
					default:
						http.NotFound(w, r)
					}
				}))
				defer upstream.Close()
				mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
				response, body := f.do(t, "GET", "/repository/proxy/simple/widget/", nil, nil)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("index = %d %s", response.StatusCode, body)
				}
				path := fmt.Sprintf("/repository/proxy/files/http/%s/widget-1.0.whl", strings.TrimPrefix(upstream.URL, "http://"))
				want := "wheel-correct"
				if companion {
					path += ".metadata"
					want = "metadata-correct"
				}
				response, body = f.do(t, "GET", path, nil, nil)
				if response.StatusCode == http.StatusOK {
					t.Fatalf("mismatched body accepted: %s", body)
				}
				repaired.Store(true)
				for replay := 0; replay < 2; replay++ {
					response, body = f.do(t, "GET", path, nil, nil)
					if response.StatusCode != http.StatusOK || string(body) != want {
						t.Fatalf("repaired replay %d = %d %s", replay, response.StatusCode, body)
					}
				}
				if got := downloads.Load(); got != 2 {
					t.Fatalf("upstream downloads = %d, want bad then repaired", got)
				}
			})
		}
	}
}

func TestPyPIProxyHashChangeChangesCacheIdentity(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://upstream.example"}
	path := "files/https/files.example/widget-1.0.whl"
	page := func(digest string) storedIndexes {
		return storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{
			"simple/widget/": []byte(fmt.Sprintf(`<a href="https://files.example/widget-1.0.whl#sha256=%s">widget-1.0.whl</a>`, digest)),
		}}
	}
	first, err := plugin.ResolveProxyRequest(context.Background(), repository, path, "", page(testSHA256("old")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := plugin.ResolveProxyRequest(context.Background(), repository, path, "", page(testSHA256("new")))
	if err != nil {
		t.Fatal(err)
	}
	if first.CachePath == second.CachePath || second.ExpectedDigests[0] != "sha256:"+testSHA256("new") {
		t.Fatalf("hash update reused cache: old=%+v new=%+v", first, second)
	}
	retained, err := plugin.ResolveProxyRequest(context.Background(), repository, path, "", storedIndexes{paths: []string{first.CachePath}})
	if err != nil || retained.CachePath != first.CachePath || !retained.CacheOnly {
		t.Fatalf("retained old file = %+v, %v", retained, err)
	}
	legacyPath := strings.Split(first.CachePath, ".suxen-source-sha256-")[0]
	retained, err = plugin.ResolveProxyRequest(context.Background(), repository, path, "", storedIndexes{paths: []string{legacyPath}})
	if err != nil || retained.CachePath != legacyPath || !retained.CacheOnly {
		t.Fatalf("retained pre-upgrade file = %+v, %v", retained, err)
	}
	if _, err := plugin.ResolveProxyRequest(context.Background(), repository, path, "", storedIndexes{paths: []string{first.CachePath, second.CachePath}}); err == nil {
		t.Fatal("ambiguous retained versions were served")
	}
	if _, err := plugin.ResolveProxyRequest(context.Background(), repository, path, "", storedIndexes{paths: []string{legacyPath, second.CachePath}}); err == nil {
		t.Fatal("legacy and new retained versions were mixed")
	}
}

func TestPyPIProxySelectsStrongestSupportedHash(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://upstream.example"}
	stored := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{
		"simple/widget/": []byte(fmt.Sprintf(`{"name":"widget","files":[{"filename":"widget-1.0.whl","url":"https://files.example/widget-1.0.whl","hashes":{"sha256":"%s","sha512":"%x"}}]}`, testSHA256("wheel"), sha512.Sum512([]byte("wheel")))),
	}}
	got, err := plugin.ResolveProxyRequest(context.Background(), repository, "files/https/files.example/widget-1.0.whl", "", stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ExpectedDigests) != 1 || got.ExpectedDigests[0] != fmt.Sprintf("sha512:%x", sha512.Sum512([]byte("wheel"))) {
		t.Fatalf("selected digests = %v", got.ExpectedDigests)
	}
}

func TestPyPIProxyDoesNotIgnoreMalformedSupportedHash(t *testing.T) {
	plugin := pypi.Format{}
	stored := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{
		"simple/widget/": []byte(`{"name":"widget","files":[{"filename":"widget-1.0.whl","url":"https://files.example/widget-1.0.whl","hashes":{"sha256":123,"md5":"ignored"}}]}`),
	}}
	got, err := plugin.ResolveProxyRequest(context.Background(), format.Repository{Type: "proxy", Upstream: "https://upstream.example"}, "files/https/files.example/widget-1.0.whl", "", stored)
	if err != nil || len(got.ExpectedDigests) != 1 || got.ExpectedDigests[0] != "sha256:" {
		t.Fatalf("malformed supported digest silently ignored: %+v, %v", got, err)
	}
}
