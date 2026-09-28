package pypi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPyPISignatureCompanionProxyAndGroup(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/widget/", "/simple/widget":
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			fmt.Fprint(w, `{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":"widget-1.0.tar.gz","url":"/files/widget-1.0.tar.gz?token=one","hashes":{},"gpg-sig":true},{"filename":"widget-2.0.tar.gz","url":"/files/widget-2.0.tar.gz","hashes":{},"gpg-sig":false}]}`)
		case "/files/widget-1.0.tar.gz.asc":
			hits.Add(1)
			if r.URL.RawQuery != "token=one" {
				t.Errorf("signature query=%q", r.URL.RawQuery)
			}
			fmt.Fprint(w, "signature")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	host := strings.TrimPrefix(upstream.URL, "http://")
	for _, repository := range []string{"proxy", "group"} {
		response, body := f.do(t, "GET", "/repository/"+repository+"/simple/widget/", nil, http.Header{"Accept": {"text/html"}})
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-gpg-sig="true"`) || !strings.Contains(string(body), `data-gpg-sig="false"`) {
			t.Fatalf("%s index=%d %s", repository, response.StatusCode, body)
		}
		path := "/repository/" + repository + "/files/http/" + host + "/files/widget-1.0.tar.gz.asc?token=one"
		response, body = f.do(t, "GET", path, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != "signature" {
			t.Fatalf("%s signature=%d %s", repository, response.StatusCode, body)
		}
		denied := "/repository/" + repository + "/files/http/" + host + "/files/widget-2.0.tar.gz.asc"
		response, body = f.do(t, "GET", denied, nil, nil)
		if response.StatusCode == http.StatusOK {
			t.Fatalf("%s unadvertised signature served: %s", repository, body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("signature should be cached by proxy and group; upstream hits=%d", hits.Load())
	}
}
