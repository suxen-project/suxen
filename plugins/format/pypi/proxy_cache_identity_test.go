package pypi_test

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pypi "github.com/suxen-project/suxen/plugins/format/pypi"
	"github.com/suxen-project/suxen/spi/format"
)

func TestPyPICacheKeysInvalidateLegacyPathWithoutLeakingQuery(t *testing.T) {
	plugin := pypi.Format{}
	path := "files/https/files.example/widget-1.0-py3-none-any.whl"
	stored := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{
		"simple/widget/": []byte(`<html><a href="https://files.example/widget-1.0-py3-none-any.whl">plain</a><a href="https://files.example/widget-1.0-py3-none-any.whl?token=secret-one">one</a><a href="https://files.example/widget-1.0-py3-none-any.whl?token=secret-two">two</a></html>`),
	}}
	resolve := func(query string) string {
		t.Helper()
		got, err := plugin.ResolveProxyRequest(context.Background(), format.Repository{Type: "proxy"}, path, query, stored)
		if err != nil {
			t.Fatal(err)
		}
		return got.CachePath
	}
	empty := resolve("")
	first := resolve("token=secret-one")
	second := resolve("token=secret-two")
	if empty == path || empty == first || first == second ||
		strings.Contains(first, "secret-one") || strings.Contains(second, "secret-two") {
		t.Fatalf("unsafe cache identity: empty=%q first=%q second=%q", empty, first, second)
	}
	attributes := plugin.ProjectAttributes(format.Asset{Path: first})
	if attributes["name"] != "widget" || attributes["version"] != "1.0" {
		t.Fatalf("cached asset lost PyPI coordinates: %+v", attributes)
	}
}

func TestPyPIQueryIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		order []int
	}{
		{name: "first-then-second", order: []int{0, 1}},
		{name: "second-then-first", order: []int{1, 0}},
	} {
		t.Run(test.name, func(t *testing.T) { testPyPIQueryIdentityOrder(t, test.order) })
	}
}

func testPyPIQueryIdentityOrder(t *testing.T, order []int) {
	f := newFixture(t, time.Hour)
	var one, two, missing atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/widget", "/simple/widget/":
			fmt.Fprint(w, `<html><a href="/download?file=one">widget-1.0.whl</a><a href="/download?file=two">widget-2.0.whl</a><a href="/download?file=missing">widget-3.0.whl</a></html>`)
		case "/download":
			switch r.URL.RawQuery {
			case "file=one":
				one.Add(1)
			case "file=two":
				two.Add(1)
			case "file=missing":
				missing.Add(1)
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, "artifact-"+r.URL.Query().Get("file"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": up.URL}))
	resp, b := f.do(t, "GET", "/repository/proxy/simple/widget/", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("index %d %s", resp.StatusCode, b)
	}
	matches := regexp.MustCompile(`href="([^"]*)"`).FindAllSubmatch(b, -1)
	if len(matches) != 3 {
		t.Fatalf("links %s", b)
	}
	missingLink := html.UnescapeString(string(matches[2][1]))
	for replay := 0; replay < 2; replay++ {
		resp, _ = f.do(t, "GET", strings.TrimPrefix(missingLink, f.suxen.URL), nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("negative cache replay %d = %d", replay, resp.StatusCode)
		}
	}
	for _, i := range order {
		m := matches[i]
		link := html.UnescapeString(string(m[1]))
		for replay := 0; replay < 2; replay++ {
			resp, b = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
			want := []string{"artifact-one", "artifact-two"}[i]
			if resp.StatusCode != 200 || string(b) != want {
				t.Errorf("download %d replay %d = %d %q, want %q", i, replay, resp.StatusCode, b, want)
			}
		}
	}
	if one.Load() != 1 || two.Load() != 1 || missing.Load() != 1 {
		t.Fatalf("upstream requests one=%d two=%d missing=%d", one.Load(), two.Load(), missing.Load())
	}
	altered := strings.Replace(html.UnescapeString(string(matches[0][1])), "file=one", "file=altered", 1)
	resp, _ = f.do(t, "GET", strings.TrimPrefix(altered, f.suxen.URL), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unadvertised query = %d", resp.StatusCode)
	}
}

func TestPyPIHTMLAttributes(t *testing.T) {
	for _, anchor := range []string{
		`<a href = "/files/widget-1.0.whl">widget-1.0.whl</a>`,
		`<a href=/files/widget-1.0.whl>widget-1.0.whl</a>`,
		"<A\nHREF = '/files/widget-1.0.whl?x=1&amp;y=2'>widget-1.0.whl</A>",
	} {
		t.Run(anchor, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/simple/widget", "/simple/widget/":
					fmt.Fprint(w, "<html>"+anchor+"</html>")
				case "/files/widget-1.0.whl":
					if strings.Contains(anchor, "x=1") && r.URL.RawQuery != "x=1&y=2" {
						t.Errorf("entity-bearing query changed: %q", r.URL.RawQuery)
					}
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
				if resp.StatusCode != 200 {
					t.Fatalf("index %d %s", resp.StatusCode, b)
				}
				m := regexp.MustCompile(`href="([^"]*)"`).FindSubmatch(b)
				if len(m) != 2 || string(m[1]) == "" {
					t.Fatalf("valid upstream href lost: %s", b)
				}
				link := html.UnescapeString(string(m[1]))
				resp, b = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
				if resp.StatusCode != 200 || string(b) != "artifact" {
					t.Errorf("download = %d %q, want artifact", resp.StatusCode, b)
				}
			}
		})
	}
}
