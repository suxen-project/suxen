package pypi_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPyPIProxyAndGroupRenderCachedIndexForCurrentAccept(t *testing.T) {
	for _, first := range []string{"html", "json"} {
		t.Run(first, func(t *testing.T) {
			var upstreamReads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.TrimSuffix(r.URL.Path, "/") != "/simple/widget" {
					http.NotFound(w, r)
					return
				}
				upstreamReads.Add(1)
				w.Header().Set("Vary", "Accept")
				if strings.Contains(r.Header.Get("Accept"), "json") {
					w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
					fmt.Fprint(w, `{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":"widget-1.0-py3-none-any.whl","url":"/files/widget-1.0-py3-none-any.whl"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><a href="/files/widget-1.0-py3-none-any.whl">widget-1.0-py3-none-any.whl</a></html>`)
			}))
			t.Cleanup(upstream.Close)
			f := newFixture(t, time.Hour)
			mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
			other := "json"
			if first == "json" {
				other = "html"
			}
			for _, step := range []struct{ repository, representation string }{
				{"proxy", first}, {"proxy", other}, {"group", other}, {"group", first},
			} {
				accept := "text/html"
				if step.representation == "json" {
					accept = "application/vnd.pypi.simple.v1+json"
				}
				response, body := f.do(t, http.MethodGet, "/repository/"+step.repository+"/simple/widget/", nil, http.Header{"Accept": {accept}})
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s %s = %d %s", step.repository, accept, response.StatusCode, body)
				}
				if !strings.Contains(strings.ToLower(response.Header.Get("Vary")), "accept") {
					t.Fatalf("%s %s missing Vary: Accept", step.repository, accept)
				}
				contentType := response.Header.Get("Content-Type")
				if step.representation == "json" {
					if !strings.Contains(contentType, "json") || !bytes.Contains(body, []byte(`"files"`)) {
						t.Fatalf("%s JSON response = %s %s", step.repository, contentType, body)
					}
				} else if !strings.Contains(contentType, "html") || !bytes.Contains(body, []byte("<a ")) {
					t.Fatalf("%s HTML response = %s %s", step.repository, contentType, body)
				}
			}
			if got := upstreamReads.Load(); got != 1 {
				t.Fatalf("upstream index reads = %d, want one cached body", got)
			}
		})
	}
}
