package pypi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPyPIQuarantineVetoesGroupIndexAndCachedDownloads(t *testing.T) {
	for _, members := range [][]string{{"quarantined", "active"}, {"active", "quarantined"}} {
		t.Run(strings.Join(members, "-"), func(t *testing.T) {
			f := newFixture(t, time.Hour)
			const filename = "widget-1.0-py3-none-any.whl"
			files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "active-wheel") }))
			defer files.Close()
			active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				fmt.Fprintf(w, `{"meta":{"api-version":"1.1"},"name":"widget","files":[{"filename":%q,"url":%q,"hashes":{},"size":12}]}`, filename, files.URL+"/"+filename)
			}))
			defer active.Close()
			quarantined := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				fmt.Fprint(w, `{"meta":{"api-version":"1.4"},"name":"widget","project-status":{"status":"quarantined","reason":"unsafe"},"files":[]}`)
			}))
			defer quarantined.Close()
			for _, member := range []struct{ name, upstream string }{{"active", active.URL}, {"quarantined", quarantined.URL}} {
				mustCreate(t, f.createRepository(t, map[string]any{"name": member.name, "format": "pypi", "type": "proxy", "upstream": member.upstream}))
			}
			filePath := "/files/http/" + strings.TrimPrefix(files.URL, "http://") + "/" + filename
			response, body := f.do(t, "GET", "/repository/active/simple/widget/", nil, nil)
			if response.StatusCode != 200 {
				t.Fatalf("active index: %d %s", response.StatusCode, body)
			}
			response, body = f.do(t, "GET", "/repository/active"+filePath, nil, nil)
			if response.StatusCode != 200 || string(body) != "active-wheel" {
				t.Fatalf("warm active file: %d %s", response.StatusCode, body)
			}
			mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": members}))
			for _, accept := range []string{"application/vnd.pypi.simple.v1+json", "text/html"} {
				response, body = f.do(t, "GET", "/repository/group/simple/widget/", nil, http.Header{"Accept": {accept}})
				if response.StatusCode != 200 || !strings.Contains(string(body), "quarantined") || strings.Contains(string(body), filename) {
					t.Fatalf("group %s index: %d %s", accept, response.StatusCode, body)
				}
				if strings.Contains(accept, "json") {
					var page struct {
						Files []any `json:"files"`
					}
					if err := json.Unmarshal(body, &page); err != nil || page.Files == nil || len(page.Files) != 0 {
						t.Fatalf("invalid quarantine files array: %s (%v)", body, err)
					}
				}
			}
			response, body = f.do(t, "GET", "/repository/group"+filePath, nil, nil)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("group cached file: %d %s", response.StatusCode, body)
			}
			response, body = f.do(t, "GET", "/api/v1/repositories/active/assets?prefix=files/", nil, nil)
			if response.StatusCode != 200 {
				t.Fatalf("active inventory: %d %s", response.StatusCode, body)
			}
			var inventory struct {
				Items []struct {
					ID int64 `json:"id"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &inventory); err != nil || len(inventory.Items) != 1 {
				t.Fatalf("active inventory: %s (%v)", body, err)
			}
			id := inventory.Items[0].ID
			response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", id), nil, nil)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("group cached ID download: %d %s", response.StatusCode, body)
			}
			response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=files/", nil, nil)
			if response.StatusCode != 200 {
				t.Fatalf("group inventory: %d %s", response.StatusCode, body)
			}
			var groupInventory struct {
				Items []any `json:"items"`
			}
			if err := json.Unmarshal(body, &groupInventory); err != nil || len(groupInventory.Items) != 0 {
				t.Fatalf("group inventory exposed file: %s (%v)", body, err)
			}
			response, body = f.do(t, "GET", "/repository/active"+filePath, nil, nil)
			if response.StatusCode != 200 || string(body) != "active-wheel" {
				t.Fatalf("unrelated active direct file: %d %s", response.StatusCode, body)
			}
		})
	}
}

func TestPyPIQuarantinedProxyRejectsAdvertisedFile(t *testing.T) {
	const filename = "widget-1.0-py3-none-any.whl"
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "must-not-download") }))
	defer files.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprintf(w, `{"meta":{"api-version":"1.4"},"name":"widget","project-status":{"status":"quarantined"},"files":[{"filename":%q,"url":%q,"hashes":{},"size":17}]}`, filename, files.URL+"/"+filename)
	}))
	defer index.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": index.URL}))
	response, body := f.do(t, "GET", "/repository/proxy/simple/widget/", nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
	if response.StatusCode != 200 || strings.Contains(string(body), filename) || !strings.Contains(string(body), `"files":[]`) {
		t.Fatalf("quarantined proxy index: %d %s", response.StatusCode, body)
	}
	path := "/repository/proxy/files/http/" + strings.TrimPrefix(files.URL, "http://") + "/" + filename
	response, body = f.do(t, "GET", path, nil, nil)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "pypi_quarantined_project") {
		t.Fatalf("quarantined proxy file: %d %s", response.StatusCode, body)
	}
}
