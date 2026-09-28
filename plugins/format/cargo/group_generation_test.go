package cargo_test

import (
	"crypto/sha256"
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
		if r.URL.Path == "/config.json" {
			fmt.Fprintf(w, `{"dl":"http://%s/api/v1/crates/{crate}/{version}/download"}`, r.Host)
		} else if r.URL.Path == "/wi/dg/widget" {
			fmt.Fprintf(w, `{"name":"widget","vers":"1.0.0","deps":[],"cksum":"%x","features":{},"yanked":false}`, sha256.Sum256([]byte(payload)))
		} else {
			fmt.Fprint(w, payload)
		}
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "cargo", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"proxy"}}))
	response, body := f.do(t, "GET", "/repository/group/config.json", nil, nil)
	if response.StatusCode != 200 {
		t.Fatalf("config %d %s", response.StatusCode, body)
	}
	var oldID int64
	const artifact = "dl/widget/1.0.0/download"
	for _, phase := range []bool{false, true} {
		updated.Store(phase)
		response, body := f.do(t, "GET", "/repository/group/wi/dg/widget", nil, nil)
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
			response, body = f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=dl/widget/", nil, nil)
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
	response, body = f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=dl/widget/", nil, nil)
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
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=dl/widget/", nil, nil)
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
	for _, test := range []struct{ route, want string }{
		{fmt.Sprintf("/api/v1/repositories/proxy/assets/%d/download", oldID), "old"},
		{fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", currentID), "new"},
	} {
		response, body = f.do(t, "GET", test.route, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != test.want {
			t.Fatalf("download %s: %d %q, want %q", test.route, response.StatusCode, body, test.want)
		}
	}
	updated.Store(false)
	response, body = f.do(t, "GET", "/repository/group/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restore old index: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=dl/widget/", nil, nil)
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &groupPage) != nil || len(groupPage.Items) != 1 || groupPage.Items[0].ID != oldID {
		t.Fatalf("restored group generation %d: %d %s", oldID, response.StatusCode, body)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "old" {
		t.Fatalf("restored group ID download: %d %q", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=config.json", nil, nil)
	var configPage struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &configPage) != nil || len(configPage.Items) != 1 {
		t.Fatalf("proxy config inventory: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "DELETE", fmt.Sprintf("/api/v1/repositories/proxy/assets/%d", configPage.Items[0].ID), nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete proxy config: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=dl/widget/", nil, nil)
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &groupPage) != nil || len(groupPage.Items) != 0 {
		t.Fatalf("group inventory without generation-defining config: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("group ID without generation-defining config: %d %q", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/proxy/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "old" {
		t.Fatalf("retained direct proxy ID after config deletion: %d %q", response.StatusCode, body)
	}
}
