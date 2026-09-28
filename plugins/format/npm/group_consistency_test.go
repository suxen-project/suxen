package npm_test

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A group must not publish a later member's checksum when an unavailable
// earlier member still has a tarball cached at the same public path.
func TestGroupPackumentFailsWhenEarlierMemberCannotRefresh(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	var outage atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/widget" {
			fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/archive.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte("first")))
			return
		}
		_, _ = w.Write([]byte("first"))
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "npm", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"first", "second"}}))
	body := fmt.Sprintf(`{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"shasum":"%x"}}},"_attachments":{"widget-1.0.0.tgz":{"data":%q}}}`, sha1.Sum([]byte("second")), base64.StdEncoding.EncodeToString([]byte("second")))
	response, payload := f.do(t, http.MethodPut, "/repository/second/widget", []byte(body), http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish second: %d %s", response.StatusCode, payload)
	}
	for _, path := range []string{"widget", "widget/-/widget-1.0.0.tgz"} {
		response, payload = f.do(t, http.MethodGet, "/repository/first/"+path, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("fill first %s: %d %s", path, response.StatusCode, payload)
		}
	}
	outage.Store(true)
	response, payload = f.do(t, http.MethodGet, "/repository/group/widget", nil, nil)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("partial group packument: %d %s, want 502", response.StatusCode, payload)
	}
	response, payload = f.do(t, http.MethodGet, "/repository/group/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("group tarball without current ownership: %d %s, want 502", response.StatusCode, payload)
	}
}

func TestGroupTarballFollowsRefreshedPackumentOwner(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	var removed atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/widget" {
			if removed.Load() {
				_, _ = w.Write([]byte(`{"name":"widget","versions":{}}`))
				return
			}
			fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/archive.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte("first")))
			return
		}
		_, _ = w.Write([]byte("first"))
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "npm", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"first", "second"}}))
	body := fmt.Sprintf(`{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"shasum":"%x"}}},"_attachments":{"widget-1.0.0.tgz":{"data":%q}}}`, sha1.Sum([]byte("second")), base64.StdEncoding.EncodeToString([]byte("second")))
	response, payload := f.do(t, http.MethodPut, "/repository/second/widget", []byte(body), http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish second: %d %s", response.StatusCode, payload)
	}
	for _, path := range []string{"widget", "widget/-/widget-1.0.0.tgz"} {
		response, payload = f.do(t, http.MethodGet, "/repository/first/"+path, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("fill first %s: %d %s", path, response.StatusCode, payload)
		}
	}
	removed.Store(true)
	response, payload = f.do(t, http.MethodGet, "/repository/group/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group packument: %d %s", response.StatusCode, payload)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Shasum string `json:"shasum"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha1.Sum([]byte("second")))
	if got := document.Versions["1.0.0"].Dist.Shasum; got != want {
		t.Fatalf("group packument shasum = %q, want %q", got, want)
	}
	response, payload = f.do(t, http.MethodGet, "/repository/group/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || string(payload) != "second" {
		t.Fatalf("group tarball: %d %q, want second", response.StatusCode, payload)
	}
	response, payload = f.do(t, http.MethodGet, "/repository/first/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || string(payload) != "first" {
		t.Fatalf("retained first-member tarball: %d %q", response.StatusCode, payload)
	}
	list := func(repository string) []struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		FormatPath string `json:"formatPath"`
	} {
		t.Helper()
		response, payload := f.do(t, http.MethodGet, "/api/v1/repositories/"+repository+"/assets?prefix=widget/-/", nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("list %s: %d %s", repository, response.StatusCode, payload)
		}
		var page struct {
			Items []struct {
				ID         int64  `json:"id"`
				Path       string `json:"path"`
				FormatPath string `json:"formatPath"`
			} `json:"items"`
		}
		if err := json.Unmarshal(payload, &page); err != nil {
			t.Fatal(err)
		}
		artifacts := page.Items[:0]
		for _, item := range page.Items {
			if item.FormatPath == "widget/-/widget-1.0.0.tgz" || item.Path == "widget/-/widget-1.0.0.tgz" {
				artifacts = append(artifacts, item)
			}
		}
		return artifacts
	}
	firstAssets := list("first")
	groupAssets := list("group")
	if len(firstAssets) != 1 || len(groupAssets) != 1 || firstAssets[0].ID == groupAssets[0].ID {
		t.Fatalf("first assets = %+v, group assets = %+v; want second member visible", firstAssets, groupAssets)
	}
	response, payload = f.do(t, http.MethodGet, fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", firstAssets[0].ID), nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("shadowed first ID download: %d %q", response.StatusCode, payload)
	}
	response, payload = f.do(t, http.MethodGet, fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", groupAssets[0].ID), nil, nil)
	if response.StatusCode != http.StatusOK || string(payload) != "second" {
		t.Fatalf("visible second ID download: %d %q", response.StatusCode, payload)
	}
}

func TestGroupDoesNotFallThroughWhenIndexOwnerLacksTarball(t *testing.T) {
	f := newFixture(t, time.Hour)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/widget" {
			fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"tarball":"http://%s/missing.tgz","shasum":"%x"}}}}`, r.Host, sha1.Sum([]byte("first")))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "npm", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "npm", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"first", "second"}}))
	body := fmt.Sprintf(`{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{"shasum":"%x"}}},"_attachments":{"widget-1.0.0.tgz":{"data":%q}}}`, sha1.Sum([]byte("second")), base64.StdEncoding.EncodeToString([]byte("second")))
	response, payload := f.do(t, http.MethodPut, "/repository/second/widget", []byte(body), http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish second: %d %s", response.StatusCode, payload)
	}
	response, payload = f.do(t, http.MethodGet, "/repository/group/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing first tarball: %d %q, want 404", response.StatusCode, payload)
	}
}
