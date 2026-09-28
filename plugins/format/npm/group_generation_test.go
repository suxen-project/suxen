package npm_test

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupInventoryHidesSupersededProxyGeneration(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	var updated atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := "old"
		if updated.Load() {
			payload = "new"
		}
		if r.URL.Path == "/widget" {
			fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/archive.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte(payload)))
		} else {
			fmt.Fprint(w, payload)
		}
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	var oldID int64
	const artifact = "widget/-/widget-1.0.0.tgz"
	for _, phase := range []bool{false, true} {
		updated.Store(phase)
		response, body := f.do(t, "GET", "/repository/group/widget", nil, nil)
		if response.StatusCode != 200 {
			t.Fatalf("index %d %s", response.StatusCode, body)
		}
		response, body = f.do(t, "GET", "/repository/group/"+artifact, nil, nil)
		want := "old"
		if phase {
			want = "new"
		}
		if response.StatusCode != 200 || string(body) != want {
			t.Fatalf("file %d %s", response.StatusCode, body)
		}
		if !phase {
			response, body = f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=widget/-/", nil, nil)
			var page struct {
				Items []struct {
					ID int64 `json:"id"`
				} `json:"items"`
			}
			if response.StatusCode != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 {
				t.Fatalf("inventory %d %s", response.StatusCode, body)
			}
			oldID = page.Items[0].ID
		}
	}
	response, body := f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=widget/-/", nil, nil)
	var proxyPage struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &proxyPage) != nil || len(proxyPage.Items) != 2 {
		t.Fatalf("retained proxy inventory: %d %s", response.StatusCode, body)
	}
	var currentID int64
	for _, item := range proxyPage.Items {
		if item.ID != oldID {
			currentID = item.ID
		}
	}
	if currentID == 0 {
		t.Fatalf("current proxy asset missing: %s", body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=widget/-/", nil, nil)
	var groupPage struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &groupPage) != nil || len(groupPage.Items) != 1 || groupPage.Items[0].ID != currentID {
		t.Fatalf("group inventory must contain only current generation %d: %d %s", currentID, response.StatusCode, body)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("superseded group ID download: %d %q; want 404", response.StatusCode, body)
	}
	for _, route := range []string{
		fmt.Sprintf("/api/v1/repositories/proxy/assets/%d/download", oldID),
		fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", currentID),
	} {
		response, body = f.do(t, "GET", route, nil, nil)
		want := "old"
		if route == fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", currentID) {
			want = "new"
		}
		if response.StatusCode != http.StatusOK || string(body) != want {
			t.Fatalf("download %s: %d %q, want %q", route, response.StatusCode, body, want)
		}
	}
	updated.Store(false)
	response, body = f.do(t, "GET", "/repository/group/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restore old packument: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=widget/-/", nil, nil)
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &groupPage) != nil || len(groupPage.Items) != 1 || groupPage.Items[0].ID != oldID {
		t.Fatalf("restored group generation %d: %d %s", oldID, response.StatusCode, body)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "old" {
		t.Fatalf("restored group ID download: %d %q", response.StatusCode, body)
	}
}
