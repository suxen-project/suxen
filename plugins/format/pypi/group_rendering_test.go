package pypi_test

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPyPIJSONRelativeLinksAcrossGroups(t *testing.T) {
	f := newFixture(t, time.Hour)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/widget/", "/simple/widget":
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			fmt.Fprint(w, `{"name":"widget","files":[{"filename":"widget-1.0.whl","url":"../../packages/widget-1.0.whl?sig=one#sha256=c7c5c1d70c5dec4416ab6158afd0b223ef40c29b1dc1f97ed9428b94d4cadb1c","hashes":{"sha256":"c7c5c1d70c5dec4416ab6158afd0b223ef40c29b1dc1f97ed9428b94d4cadb1c"},"core-metadata":true}]}`)
		case "/packages/widget-1.0.whl":
			if r.URL.RawQuery != "sig=one" {
				http.Error(w, "missing signature", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "artifact")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	empty := httptest.NewServer(http.NotFoundHandler())
	defer empty.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": up.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "empty", "format": "pypi", "type": "proxy", "upstream": empty.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "single", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "mixed", "format": "pypi", "type": "group", "members": []string{"empty", "proxy"}}))
	for _, repo := range []string{"single", "mixed"} {
		response, body := f.do(t, "GET", "/repository/"+repo+"/simple/widget/", nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s index = %d %s", repo, response.StatusCode, body)
		}
		var page struct {
			Files []struct {
				URL    string            `json:"url"`
				Hashes map[string]string `json:"hashes"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &page); err != nil || len(page.Files) != 1 {
			t.Fatalf("%s page = %s, %v", repo, body, err)
		}
		link := page.Files[0].URL
		if !strings.HasPrefix(link, f.suxen.URL+"/repository/"+repo+"/files/") ||
			!strings.Contains(link, "?sig=one") || strings.Contains(link, "#") ||
			page.Files[0].Hashes["sha256"] != "c7c5c1d70c5dec4416ab6158afd0b223ef40c29b1dc1f97ed9428b94d4cadb1c" {
			t.Fatalf("%s rewritten link or hashes = %s", repo, body)
		}
		response, body = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != "artifact" {
			t.Fatalf("%s download = %d %s", repo, response.StatusCode, body)
		}
	}
}

func TestPyPIEscapedSignedFilenames(t *testing.T) {
	for _, escaped := range []string{"widget-1.0%2Blocal.whl", "widget%20one-1.0.whl", "widget-%E2%98%83-1.0.whl"} {
		t.Run(escaped, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/simple/widget/", "/simple/widget":
					fmt.Fprintf(w, `<html><a href="/files/%s?sig=one">file</a></html>`, escaped)
				default:
					if r.URL.EscapedPath() != "/files/"+escaped || r.URL.RawQuery != "sig=one" {
						http.NotFound(w, r)
						return
					}
					fmt.Fprint(w, "artifact")
				}
			}))
			defer up.Close()
			mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": up.URL}))
			response, body := f.do(t, "GET", "/repository/proxy/simple/widget/", nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("index = %d %s", response.StatusCode, body)
			}
			match := regexp.MustCompile(`href="([^"]*)"`).FindSubmatch(body)
			if len(match) != 2 {
				t.Fatalf("no link: %s", body)
			}
			link := html.UnescapeString(string(match[1]))
			response, body = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
			if response.StatusCode != http.StatusOK || string(body) != "artifact" {
				t.Fatalf("signed download = %d %s", response.StatusCode, body)
			}
			response, _ = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL)+"&sig=altered", nil, nil)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("altered signed link = %d, want 400", response.StatusCode)
			}
		})
	}
}

func TestPyPIRelativeGroup(t *testing.T) {
	f := newFixture(t, time.Hour)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/widget/", "/simple/widget":
			fmt.Fprint(w, `<html><a href="../../packages/widget-1.0.whl">widget-1.0.whl</a></html>`)
		case "/packages/widget-1.0.whl":
			fmt.Fprint(w, "artifact")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": up.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	for _, repo := range []string{"proxy", "group"} {
		resp, b := f.do(t, "GET", "/repository/"+repo+"/simple/widget/", nil, nil)
		t.Logf("%s index %d %s", repo, resp.StatusCode, b)
		m := regexp.MustCompile(`href="([^"]*)"`).FindSubmatch(b)
		if len(m) < 2 {
			t.Fatal("no href")
		}
		link := html.UnescapeString(string(m[1]))
		if link == "" {
			t.Errorf("%s emitted an empty download href", repo)
			continue
		}
		resp, b = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
		if resp.StatusCode != 200 || string(b) != "artifact" {
			t.Errorf("%s file %d %s", repo, resp.StatusCode, b)
		}
	}
}
