package pypi_test

import (
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPyPIMetadataAdvertisementAcrossRepresentations(t *testing.T) {
	cases := []struct {
		name      string
		metadata  string
		attribute string
	}{
		{"modern-false", `"core-metadata":false`, ""},
		{"legacy-false", `"dist-info-metadata":false`, ""},
		{"modern-false-overrides-legacy-true", `"core-metadata":false,"dist-info-metadata":true`, ""},
		{"modern-false-overrides-legacy-hash", `"core-metadata":false,"dist-info-metadata":{"sha256":"legacy"}`, ""},
		{"modern-true", `"core-metadata":true`, `data-core-metadata="true"`},
		{"legacy-true", `"dist-info-metadata":true`, `data-dist-info-metadata="true"`},
		{"modern-hash-overrides-legacy", `"core-metadata":{"sha256":"modern"},"dist-info-metadata":{"sha256":"legacy"}`, `data-core-metadata="sha256=modern"`},
		{"modern-strongest-hash", `"core-metadata":{"sha256":"weaker","sha512":"stronger"}`, `data-core-metadata="sha512=stronger"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			var companionHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/widget-1.0-py3-none-any.whl" {
					fmt.Fprint(w, "wheel bytes")
					return
				}
				if r.URL.Path != "/simple/widget/" && r.URL.Path != "/simple/widget" {
					if strings.HasSuffix(r.URL.Path, ".metadata") {
						companionHits.Add(1)
					}
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				fmt.Fprintf(w, `{"name":"widget","files":[{"filename":"widget-1.0-py3-none-any.whl","url":"/widget-1.0-py3-none-any.whl",%s}]}`, tc.metadata)
			}))
			defer upstream.Close()
			mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))

			for _, repository := range []string{"proxy", "group"} {
				indexPath := "/repository/" + repository + "/simple/widget/"
				response, body := f.do(t, http.MethodGet, indexPath, nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s JSON index = %d %s", repository, response.StatusCode, body)
				}
				var document struct {
					Files []map[string]any `json:"files"`
				}
				if err := json.Unmarshal(body, &document); err != nil || len(document.Files) != 1 {
					t.Fatalf("%s JSON index = %s, %v", repository, body, err)
				}
				if strings.Contains(tc.metadata, `"core-metadata":false`) && document.Files[0]["core-metadata"] != false {
					t.Fatalf("%s lost modern false flag: %s", repository, body)
				}
				if strings.Contains(tc.metadata, `"dist-info-metadata":false`) && document.Files[0]["dist-info-metadata"] != false {
					t.Fatalf("%s lost legacy false flag: %s", repository, body)
				}

				response, body = f.do(t, http.MethodGet, indexPath, nil, http.Header{"Accept": {"text/html"}})
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s HTML index = %d %s", repository, response.StatusCode, body)
				}
				html := string(body)
				if tc.attribute == "" {
					if strings.Contains(html, "data-core-metadata") || strings.Contains(html, "data-dist-info-metadata") {
						t.Fatalf("%s advertised unavailable metadata: %s", repository, html)
					}
					companion := "/repository/" + repository + "/files/http/" + strings.TrimPrefix(upstream.URL, "http://") + "/widget-1.0-py3-none-any.whl.metadata"
					response, body = f.do(t, http.MethodGet, companion, nil, nil)
					if response.StatusCode == http.StatusOK {
						t.Fatalf("%s served unavailable metadata: %s", repository, body)
					}
					if got := companionHits.Load(); got != 0 {
						t.Fatalf("%s fetched unavailable metadata %d times", repository, got)
					}
					// A client that requests HTML and checks for the metadata
					// attribute must fall back to downloading the wheel.
					match := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(html)
					if len(match) != 2 {
						t.Fatalf("%s HTML index has no wheel link: %s", repository, html)
					}
					wheelURL := htmlpkg.UnescapeString(match[1])
					response, body = f.do(t, http.MethodGet, strings.TrimPrefix(wheelURL, f.suxen.URL), nil, nil)
					if response.StatusCode != http.StatusOK || string(body) != "wheel bytes" {
						t.Fatalf("%s fallback wheel = %d %s", repository, response.StatusCode, body)
					}
				} else if !strings.Contains(html, tc.attribute) || strings.Count(html, "data-core-metadata")+strings.Count(html, "data-dist-info-metadata") != 1 {
					t.Fatalf("%s metadata attribute = %s, want %s", repository, html, tc.attribute)
				}
			}
		})
	}
}
