package pypi_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPyPIGroupFailsClosedWhenMemberIndexRefreshFails(t *testing.T) {
	f := newFixture(t, 10*time.Millisecond)
	var outage, replaced atomic.Bool
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if replaced.Load() {
			fmt.Fprint(w, "new-wheel")
		} else {
			fmt.Fprint(w, "old-wheel")
		}
	}))
	defer files.Close()
	const filename = "widget-1.0-py3-none-any.whl"
	page := func(payload string) string {
		return fmt.Sprintf(`{"name":"widget","files":[{"filename":%q,"url":%q,"hashes":{"sha256":"%x"}}]}`,
			filename, files.URL+"/"+filename, sha256.Sum256([]byte(payload)))
	}
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprint(w, page("old-wheel"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprint(w, page("new-wheel"))
	}))
	defer second.Close()
	for _, member := range []struct{ name, upstream string }{{"first", first.URL}, {"second", second.URL}} {
		mustCreate(t, f.createRepository(t, map[string]any{"name": member.name, "format": "pypi", "type": "proxy", "upstream": member.upstream}))
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"first", "second"}}))
	response, body := f.do(t, "GET", "/repository/first/simple/widget/", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("warm index: %d %s", response.StatusCode, body)
	}
	filePath := "/files/http/" + strings.TrimPrefix(files.URL, "http://") + "/" + filename
	response, body = f.do(t, "GET", "/repository/first"+filePath, nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "old-wheel" {
		t.Fatalf("warm file: %d %s", response.StatusCode, body)
	}
	outage.Store(true)
	replaced.Store(true)
	time.Sleep(30 * time.Millisecond)
	for _, path := range []string{"/simple/widget/", filePath} {
		response, body = f.do(t, "GET", "/repository/group"+path, nil, nil)
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("group %s during outage: %d %s", path, response.StatusCode, body)
		}
	}
}

func TestPyPIGroupFilenameOwnerControlsURLAndInventory(t *testing.T) {
	f := newFixture(t, time.Hour)
	const filename = "widget-1.0-py3-none-any.whl"
	firstFiles := httptest.NewServer(http.NotFoundHandler())
	defer firstFiles.Close()
	secondFiles := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "second-wheel") }))
	defer secondFiles.Close()
	index := func(fileURL string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			fmt.Fprintf(w, `{"name":"widget","files":[{"filename":%q,"url":%q}]}`, filename, fileURL)
		}))
	}
	first := index(firstFiles.URL + "/missing/" + filename)
	defer first.Close()
	second := index(secondFiles.URL + "/download?token=second")
	defer second.Close()
	for _, member := range []struct{ name, upstream string }{{"first", first.URL}, {"second", second.URL}} {
		mustCreate(t, f.createRepository(t, map[string]any{"name": member.name, "format": "pypi", "type": "proxy", "upstream": member.upstream}))
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"first", "second"}}))
	response, body := f.do(t, "GET", "/repository/group/simple/widget/", nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group index: %d %s", response.StatusCode, body)
	}
	var page struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Files) != 1 || !strings.Contains(page.Files[0].URL, firstFiles.URL[7:]) {
		t.Fatalf("group owner index: %s (%v)", body, err)
	}
	secondPath := "/files/http/" + strings.TrimPrefix(secondFiles.URL, "http://") + "/download?token=second"
	response, body = f.do(t, "GET", "/repository/second"+secondPath, nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "second-wheel" {
		t.Fatalf("second cache warm: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/repository/group"+secondPath, nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("shadowed second URL: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=files/", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group inventory: %d %s", response.StatusCode, body)
	}
	var inventory struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &inventory); err != nil || len(inventory.Items) != 0 {
		t.Fatalf("shadowed generation in group inventory: %s (%v)", body, err)
	}
	response, body = f.do(t, "GET", "/api/v1/repositories/second/assets?prefix=files/", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("second inventory: %d %s", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &inventory); err != nil || len(inventory.Items) != 1 {
		t.Fatalf("second inventory: %s (%v)", body, err)
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", inventory.Items[0].ID), nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("shadowed ID download: %d %s", response.StatusCode, body)
	}
}

func TestPyPIGroupHidesSupersededGenerationAndServesMetadataCompanion(t *testing.T) {
	f := newFixture(t, 10*time.Millisecond)
	var updated atomic.Bool
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "token=signed" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/download.metadata" {
			fmt.Fprint(w, "Metadata-Version: 2.3\nName: widget\n")
			return
		}
		if updated.Load() {
			fmt.Fprint(w, "new-wheel")
		} else {
			fmt.Fprint(w, "old-wheel")
		}
	}))
	defer files.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := "old-wheel"
		if updated.Load() {
			payload = "new-wheel"
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprintf(w, `{"name":"widget","files":[{"filename":"widget-1.0-py3-none-any.whl","url":%q,"hashes":{"sha256":"%x"},"core-metadata":true}]}`,
			files.URL+"/download?token=signed", sha256.Sum256([]byte(payload)))
	}))
	defer index.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": index.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	filePath := "/files/http/" + strings.TrimPrefix(files.URL, "http://") + "/download?token=signed"
	var oldID int64
	for _, phase := range []struct {
		updated bool
		want    string
	}{{false, "old-wheel"}, {true, "new-wheel"}} {
		updated.Store(phase.updated)
		response, body := f.do(t, "GET", "/repository/group/simple/widget/", nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("phase %v index: %d %s", phase.updated, response.StatusCode, body)
		}
		var page struct {
			Files []struct {
				URL string `json:"url"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &page); err != nil || len(page.Files) != 1 || !strings.Contains(page.Files[0].URL, "?token=signed") {
			t.Fatalf("phase %v index: %s (%v)", phase.updated, body, err)
		}
		response, body = f.do(t, "GET", "/repository/group"+filePath, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != phase.want {
			t.Fatalf("phase %v wheel: %d %s", phase.updated, response.StatusCode, body)
		}
		response, body = f.do(t, "GET", "/repository/group"+strings.Replace(filePath, "/download?", "/download.metadata?", 1), nil, nil)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Name: widget") {
			t.Fatalf("phase %v metadata: %d %s", phase.updated, response.StatusCode, body)
		}
		if !phase.updated {
			response, body = f.do(t, "GET", "/api/v1/repositories/proxy/assets?prefix=files/", nil, nil)
			var stored struct {
				Items []struct {
					ID         int64  `json:"id"`
					FormatPath string `json:"formatPath"`
				} `json:"items"`
			}
			if response.StatusCode != http.StatusOK || json.Unmarshal(body, &stored) != nil {
				t.Fatalf("old inventory: %d %s", response.StatusCode, body)
			}
			for _, item := range stored.Items {
				if strings.HasSuffix(item.FormatPath, "/download") {
					oldID = item.ID
				}
			}
			if oldID == 0 {
				t.Fatalf("old wheel missing from inventory: %s", body)
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	response, body := f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=files/", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group inventory: %d %s", response.StatusCode, body)
	}
	var inventory struct {
		Items []struct {
			ID         int64  `json:"id"`
			FormatPath string `json:"formatPath"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &inventory); err != nil || len(inventory.Items) != 2 {
		t.Fatalf("expected current wheel and metadata only: %s (%v)", body, err)
	}
	for _, item := range inventory.Items {
		if item.ID == oldID {
			t.Fatalf("superseded wheel visible in group inventory: %s", body)
		}
	}
	response, body = f.do(t, "GET", fmt.Sprintf("/api/v1/repositories/group/assets/%d/download", oldID), nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("superseded wheel ID download: %d %s", response.StatusCode, body)
	}
}

func TestPyPIGroupSelfProxyTracksHostedOverwrite(t *testing.T) {
	f := newFixture(t, 10*time.Millisecond)
	const filename = "widget-1.0-py3-none-any.whl"
	mustCreate(t, f.createRepository(t, map[string]any{"name": "source", "format": "pypi", "type": "hosted", "allowOverwrite": true}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": f.suxen.URL + "/repository/source"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	for _, payload := range []string{"old-wheel", "new-wheel"} {
		response, body := twineUpload(t, f, "source", "widget", "1.0", filename, []byte(payload))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("upload %s: %d %s", payload, response.StatusCode, body)
		}
		time.Sleep(30 * time.Millisecond)
		response, body = f.do(t, "GET", "/repository/group/simple/widget/", nil,
			http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("group index %s: %d %s", payload, response.StatusCode, body)
		}
		var page struct {
			Files []struct {
				URL string `json:"url"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &page); err != nil || len(page.Files) != 1 || !strings.Contains(page.Files[0].URL, "/repository/group/packages/") {
			t.Fatalf("group proxy link %s: %s (%v)", payload, body, err)
		}
		response, body = f.do(t, "GET", strings.TrimPrefix(page.Files[0].URL, f.suxen.URL), nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != payload {
			t.Fatalf("group download %s: %d %s", payload, response.StatusCode, body)
		}
	}
}

func TestPyPIHostedGroupUsesSynthesizedIndexOwner(t *testing.T) {
	f := newFixture(t, time.Hour)
	const filename = "widget-1.0-py3-none-any.whl"
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"hosted"}}))
	response, body := twineUpload(t, f, "hosted", "widget", "1.0", filename, []byte("hosted-wheel"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("hosted upload: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, "GET", "/repository/group/simple/widget/", nil,
		http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("hosted group index: %d %s", response.StatusCode, body)
	}
	var page struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Files) != 1 {
		t.Fatalf("hosted group page: %s (%v)", body, err)
	}
	response, body = f.do(t, "GET", strings.TrimPrefix(page.Files[0].URL, f.suxen.URL), nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "hosted-wheel" {
		t.Fatalf("hosted group wheel: %d %s", response.StatusCode, body)
	}
}
