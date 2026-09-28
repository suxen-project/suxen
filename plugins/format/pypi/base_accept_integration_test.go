package pypi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPyPIBaseProxyAndGroupFetch(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/packages/widget-1.0.whl" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "wheel")
	}))
	defer cdn.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSuffix(r.URL.Path, "/") != "/simple/widget" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><base href="%s/packages/"></head><body><a href="widget-1.0.whl">widget-1.0.whl</a></body></html>`, cdn.URL)
	}))
	defer index.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": index.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	for _, repo := range []string{"proxy", "group"} {
		response, body := f.do(t, "GET", "/repository/"+repo+"/simple/widget/", nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s index=%d %s", repo, response.StatusCode, body)
		}
		path := "/repository/" + repo + "/files/http/" + strings.TrimPrefix(cdn.URL, "http://") + "/packages/widget-1.0.whl"
		if !strings.Contains(string(body), path) {
			t.Fatalf("%s index missed CDN: %s", repo, body)
		}
		response, body = f.do(t, "GET", path, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != "wheel" {
			t.Fatalf("%s CDN fetch=%d %s", repo, response.StatusCode, body)
		}
	}
}

func TestPyPIAcceptAcrossHostedProxyGroup(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSuffix(r.URL.Path, "/") != "/simple" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprint(w, `{"meta":{"api-version":"1.0"},"projects":[{"name":"widget"}]}`)
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	cases := []struct {
		accept, media string
		first         byte
	}{
		{"application/vnd.pypi.simple.v1+json", "application/vnd.pypi.simple.v1+json", '{'},
		{"application/vnd.pypi.simple.latest+json", "application/vnd.pypi.simple.v1+json", '{'},
		{"application/vnd.pypi.simple.v1+html", "application/vnd.pypi.simple.v1+html", '<'},
		{"application/*", "application/vnd.pypi.simple.v1+json", '{'},
		{"text/html, application/vnd.pypi.simple.v1+json;q=0", "text/html", '<'},
		{"application/vnd.pypi.simple.v1+json;q=0, */*", "text/html", '<'},
		{"application/example+json, text/html", "text/html", '<'},
		{"application/example+json", "text/html", '<'},
		{"application/example+json, application/vnd.pypi.simple.v1+json;q=0", "text/html", '<'},
	}
	for _, repo := range []string{"hosted", "proxy", "group"} {
		for _, tc := range cases {
			t.Run(repo+"/"+tc.accept, func(t *testing.T) {
				response, body := f.do(t, "GET", "/repository/"+repo+"/simple/", nil, http.Header{"Accept": {tc.accept}})
				if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), tc.media) || len(body) == 0 || body[0] != tc.first {
					t.Fatalf("status=%d type=%s body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
				}
			})
		}
		response, body := f.do(t, "GET", "/repository/"+repo+"/simple/", nil,
			http.Header{"Accept": {"text/html;q=0.1", "application/vnd.pypi.simple.v1+json;q=0.9"}})
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/vnd.pypi.simple.v1+json") || len(body) == 0 || body[0] != '{' {
			t.Fatalf("%s repeated Accept: %d %s %s", repo, response.StatusCode, response.Header.Get("Content-Type"), body)
		}
	}
}
