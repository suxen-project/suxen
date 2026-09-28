package npm_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemovedVersionRetainsCachedTarball(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	var removed atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/widget":
			if removed.Load() {
				fmt.Fprint(w, `{"name":"widget","versions":{}}`)
				return
			}
			fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"dist":{"tarball":"http://%s/archive.tgz"}}}}`, r.Host)
		case "/archive.tgz":
			fmt.Fprint(w, "cached-artifact")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL,
	}))
	for _, path := range []string{"widget", "widget/-/widget-1.0.0.tgz"} {
		response, body := f.do(t, "GET", "/repository/proxy/"+path, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("fill %s: %d %s", path, response.StatusCode, body)
		}
	}

	removed.Store(true)
	response, body := f.do(t, "GET", "/repository/proxy/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("refresh packument: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/repository/proxy/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "cached-artifact" {
		t.Fatalf("retained tarball: %d %s", response.StatusCode, body)
	}
}
