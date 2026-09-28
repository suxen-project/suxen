package npm_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The version count and per-version fields model a high-churn registry
// packument. Generating the fixture keeps the test hermetic without checking
// a multi-megabyte upstream response into the repository.
func largePackument(t *testing.T, packageName, tarballBase string) []byte {
	t.Helper()
	versions := make(map[string]any, 6800)
	readme := strings.Repeat("release notes for this package and its dependencies; ", 25)
	for number := range 6800 {
		version := fmt.Sprintf("1.%d.0", number)
		versions[version] = map[string]any{
			"name": packageName, "version": version,
			"description":  readme,
			"dependencies": map[string]string{"@types/node": "^20.0.0", "typescript": "^5.0.0"},
			"dist":         map[string]string{"tarball": fmt.Sprintf("%s/%s/-/%s-%s.tgz", tarballBase, packageName, packageName, version)},
		}
	}
	content, err := json.Marshal(map[string]any{
		"name": packageName, "dist-tags": map[string]string{"latest": "1.6799.0"},
		"versions": versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(content) <= 8<<20 || len(content) >= 16<<20 {
		t.Fatalf("generated packument size = %d, want between 8 and 16 MiB", len(content))
	}
	return content
}

func TestLargePackumentProxyGroupAndTarball(t *testing.T) {
	f := newFixture(t, time.Hour)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(largePackument(t, "large", upstream.URL))
			return
		}
		if r.URL.Path == "/large/-/large-1.0.0.tgz" {
			_, _ = w.Write([]byte("upstream-tarball"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "large-proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL,
	}))
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "large-group", "format": "npm", "type": "group", "members": []string{"large-proxy"},
	}))

	for _, path := range []string{"/repository/large-proxy/large", "/repository/large-group/large"} {
		response, body := f.do(t, http.MethodGet, path, nil, nil)
		if response.StatusCode != http.StatusOK || len(body) <= 8<<20 {
			t.Fatalf("%s: status %d, body %d bytes", path, response.StatusCode, len(body))
		}
		if !bytes.Contains(body, []byte("/repository/")) || bytes.Contains(body, []byte(upstream.URL+"/large/-/")) {
			t.Fatalf("%s: tarball URLs were not rewritten", path)
		}
	}
	for _, path := range []string{
		"/repository/large-proxy/large/-/large-1.0.0.tgz",
		"/repository/large-group/large/-/large-1.0.0.tgz",
	} {
		response, body := f.do(t, http.MethodGet, path, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != "upstream-tarball" {
			t.Fatalf("%s: status %d, body %q", path, response.StatusCode, body)
		}
	}
	response, body := f.do(t, http.MethodGet, "/api/v1/repositories/large-group/assets?prefix=large/-/", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("large/-/large-1.0.0.tgz")) {
		t.Fatalf("group inventory: status %d, body %d bytes", response.StatusCode, len(body))
	}
}

func TestLargeHostedPackumentContributesToGroup(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "large-hosted", "format": "npm", "type": "hosted"}))
	proxy := newRegistry(t, map[string][]string{"large": {"3.0.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "small-proxy", "format": "npm", "type": "proxy", "upstream": proxy.metadata.URL,
	}))
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "hosted-proxy-group", "format": "npm", "type": "group", "members": []string{"large-hosted", "small-proxy"},
	}))

	versions := make(map[string]any, 2)
	attachments := make(map[string]any, 2)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		file := "large-" + version + ".tgz"
		archive := tarball(t, "large", version)
		versions[version] = map[string]any{
			"name": "large", "version": version,
			"readme": strings.Repeat("release notes for published version\n", 140000),
			"dist":   map[string]any{"tarball": "http://publisher/large/-/" + file},
		}
		attachments[file] = map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(archive), "length": len(archive),
		}
	}
	publish, err := json.Marshal(map[string]any{"name": "large", "versions": versions, "_attachments": attachments})
	if err != nil {
		t.Fatal(err)
	}
	response, body := f.do(t, http.MethodPut, "/repository/large-hosted/large", publish, http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish: %d (%d bytes)", response.StatusCode, len(body))
	}
	for _, path := range []string{"/repository/large-hosted/large", "/repository/hosted-proxy-group/large"} {
		response, body = f.do(t, http.MethodGet, path, nil, nil)
		if response.StatusCode != http.StatusOK || len(body) <= 8<<20 {
			t.Fatalf("%s: status %d, body %d bytes", path, response.StatusCode, len(body))
		}
		if !bytes.Contains(body, []byte(`"3.0.0"`)) && strings.Contains(path, "group") {
			t.Fatalf("%s: proxy version missing from merged packument", path)
		}
	}
}
