package npm_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
)

const npmInstallAccept = "application/vnd.npm.install-v1+json"

func TestNpmProxyUsesFullPackumentForEitherClientOrder(t *testing.T) {
	for _, firstAccept := range []string{npmInstallAccept, "application/json"} {
		t.Run(firstAccept, func(t *testing.T) {
			var mu sync.Mutex
			var accepts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/widget":
					mu.Lock()
					accepts = append(accepts, r.Header.Get("Accept"))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					fmt.Fprintf(w, `{"name":"widget","description":"full description","readme":"full readme","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/archive.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte("archive")))
				case "/archive.tgz":
					fmt.Fprint(w, "archive")
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			f := newFixture(t, time.Hour)
			mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
			for _, request := range []struct{ repository, accept string }{
				{"proxy", firstAccept},
				{"proxy", "application/json"},
				{"group", npmInstallAccept},
				{"group", "application/json"},
			} {
				response, body := f.do(t, http.MethodGet, "/repository/"+request.repository+"/widget", nil, http.Header{"Accept": {request.accept}})
				if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"readme":"full readme"`)) || !bytes.Contains(body, []byte(`"description":"full description"`)) {
					t.Fatalf("%s Accept=%s: %d %s", request.repository, request.accept, response.StatusCode, body)
				}
			}
			response, body := f.do(t, http.MethodGet, "/repository/group/widget/-/widget-1.0.0.tgz", nil, nil)
			if response.StatusCode != http.StatusOK || string(body) != "archive" {
				t.Fatalf("group tarball: %d %q", response.StatusCode, body)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(accepts) != 1 || accepts[0] != "application/json" {
				t.Fatalf("upstream packument Accept headers = %q", accepts)
			}
		})
	}
}

func TestNpmProxyFullPackumentRevalidation(t *testing.T) {
	var generation atomic.Int32
	var mu sync.Mutex
	var headers []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("Accept") != "application/json" {
			w.Header().Set("Content-Type", npmInstallAccept)
			fmt.Fprint(w, `{"name":"widget","versions":{}}`)
			return
		}
		body := fmt.Sprintf(`{"name":"widget","readme":"generation %d","versions":{}}`, generation.Load())
		digest := sha256.Sum256([]byte(body))
		if r.Header.Get("If-None-Match") == fmt.Sprintf(`"sha256:%x"`, digest) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer upstream.Close()
	f := newFixture(t, time.Nanosecond)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	for _, want := range []string{"generation 0", "generation 1", "generation 1"} {
		if want == "generation 1" {
			generation.Store(1)
		}
		response, body := f.do(t, http.MethodGet, "/repository/proxy/widget", nil, http.Header{"Accept": {npmInstallAccept}})
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Fatalf("refresh %s: %d %s", want, response.StatusCode, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 3 || headers[2].Get("If-None-Match") == "" {
		t.Fatalf("revalidation headers = %+v", headers)
	}
	for _, header := range headers {
		if header.Get("Accept") != "application/json" {
			t.Fatalf("upstream Accept = %q", header.Get("Accept"))
		}
	}
}

func TestNpmProxyReplacesLegacyAbbreviatedCacheWithoutValidator(t *testing.T) {
	var mu sync.Mutex
	var requests []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive.tgz" {
			fmt.Fprint(w, "archive")
			return
		}
		mu.Lock()
		requests = append(requests, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":"widget","readme":"full readme","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/archive.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte("archive")))
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	response, body := f.do(t, http.MethodGet, "/repository/proxy/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("warm full packument: %d %s", response.StatusCode, body)
	}
	legacy := []byte(fmt.Sprintf(`{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"%s/archive.tgz","shasum":"%x"}}}}`, upstream.URL, sha1.Sum([]byte("archive"))))
	legacyDigest := sha256.Sum256(legacy)
	blobStore, err := blob.NewFS(filepath.Join(f.dataDirectory, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobStore.Put(context.Background(), fmt.Sprintf("sha256:%x", legacyDigest), bytes.NewReader(legacy)); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(f.dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`UPDATE assets SET digest=?, size=?, content_type=? WHERE path='widget'`, fmt.Sprintf("sha256:%x", legacyDigest), len(legacy), npmInstallAccept+"; charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	response, body = f.do(t, http.MethodGet, "/repository/group/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "archive" {
		t.Fatalf("tarball from legacy metadata: %d %q", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/group/widget", nil, http.Header{"Accept": {npmInstallAccept}})
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"readme":"full readme"`)) {
		t.Fatalf("group upgrade of legacy metadata: %d %s", response.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[1].Get("Accept") != "application/json" || requests[1].Get("If-None-Match") != "" {
		t.Fatalf("legacy replacement request headers = %+v", requests)
	}
}

func TestNpmProxyAcceptsHeaderlessFullPackument(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Accept"); got != "application/json" {
			http.Error(w, "wrong Accept: "+got, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"name":"widget","readme":"full readme","versions":{}}`)
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	for _, accept := range []string{npmInstallAccept, "application/json"} {
		response, body := f.do(t, http.MethodGet, "/repository/proxy/widget", nil, http.Header{"Accept": {accept}})
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"readme":"full readme"`)) || response.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Accept %s: status=%d type=%s body=%s", accept, response.StatusCode, response.Header.Get("Content-Type"), body)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want one canonical fill", got)
	}
}

func TestNpmProxyRejectsAbbreviatedUpstreamResponse(t *testing.T) {
	var abbreviated atomic.Bool
	abbreviated.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if abbreviated.Load() {
			w.Header().Set("Content-Type", npmInstallAccept)
			fmt.Fprint(w, `{"name":"widget","versions":{}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"widget","readme":"full readme","versions":{}}`)
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	response, body := f.do(t, http.MethodGet, "/repository/proxy/widget", nil, http.Header{"Accept": {npmInstallAccept}})
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("abbreviated upstream representation: %d %s", response.StatusCode, body)
	}
	abbreviated.Store(false)
	response, body = f.do(t, http.MethodGet, "/repository/proxy/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"readme":"full readme"`)) {
		t.Fatalf("full metadata after rejected response: %d %s", response.StatusCode, body)
	}
}
