package pypi_test

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPyPIProxyRetainsCachedDistributionAfterIndexDeletion(t *testing.T) {
	fixture := newFixture(t, time.Hour)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		switch r.URL.Path {
		case "/simple/widget/", "/simple/widget":
			fmt.Fprint(w, `<a href="/widget-1.0.whl?token=first">widget-1.0.whl</a>`)
		case "/widget-1.0.whl":
			if r.URL.RawQuery != "token=first" {
				t.Errorf("upstream query = %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, "cached-wheel")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	mustCreate(t, fixture.createRepository(t, map[string]any{
		"name": "retained", "format": "pypi", "type": "proxy", "upstream": upstream.URL,
	}))

	response, body := fixture.do(t, http.MethodGet, "/repository/retained/simple/widget/", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("project index = %d %s", response.StatusCode, body)
	}
	match := regexp.MustCompile(`href="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("project index has no distribution link: %s", body)
	}
	filePath := strings.TrimPrefix(html.UnescapeString(string(match[1])), fixture.suxen.URL)
	response, body = fixture.do(t, http.MethodGet, filePath, nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "cached-wheel" {
		t.Fatalf("cache fill = %d %s", response.StatusCode, body)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls after fill = %d, want 2", got)
	}

	response, body = fixture.do(t, http.MethodGet, "/api/v1/repositories/retained/assets?prefix=simple", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list project index = %d %s", response.StatusCode, body)
	}
	var page struct {
		Items []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "simple/widget" {
		t.Fatalf("project index assets = %s", body)
	}
	response, body = fixture.do(t, http.MethodDelete,
		fmt.Sprintf("/api/v1/repositories/retained/assets/%d", page.Items[0].ID), nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete project index = %d %s", response.StatusCode, body)
	}

	response, body = fixture.do(t, http.MethodGet, filePath, nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "cached-wheel" {
		t.Fatalf("retained cache read = %d %s", response.StatusCode, body)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls after retained read = %d, want 2", got)
	}
}
