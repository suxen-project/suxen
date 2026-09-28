package pypi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPyPIJSONRequiredShapesAcrossRepositories(t *testing.T) {
	f := newFixture(t, time.Hour)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/simple/", "/simple":
			fmt.Fprint(w, `<html><a href="/simple/widget/">widget</a></html>`)
		case "/simple/widget/", "/simple/widget":
			fmt.Fprint(w, `<html><a href="https://files.example/widget-1.0.tar.gz">widget-1.0.tar.gz</a></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	accept := http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}}
	for _, test := range []struct {
		repository string
		path       string
		count      int
	}{
		{"hosted", "simple/", 0},
		{"proxy", "simple/", 1},
		{"group", "simple/", 1},
		{"proxy", "simple/widget/", 1},
		{"group", "simple/widget/", 1},
	} {
		t.Run(test.repository+"/"+test.path, func(t *testing.T) {
			response, body := f.do(t, http.MethodGet, "/repository/"+test.repository+"/"+test.path, nil, accept)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("index = %d %s", response.StatusCode, body)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(body, &document); err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal(document["meta"], &meta); err != nil || meta["api-version"] != "1.0" {
				t.Fatalf("meta object missing: %s %v", body, err)
			}
			key := "projects"
			if test.path != "simple/" {
				key = "files"
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(document[key], &entries); err != nil || entries == nil || len(entries) != test.count {
				t.Fatalf("%s array missing: %s %v", key, body, err)
			}
			if key == "files" {
				var hashes map[string]string
				if err := json.Unmarshal(entries[0]["hashes"], &hashes); err != nil || hashes == nil || len(hashes) != 0 {
					t.Fatalf("hashless file needs empty hashes object: %s %v", body, err)
				}
			}
		})
	}
	response, body := twineUpload(t, f, "hosted", "widget", "1.0", "widget-1.0-py3-none-any.whl", []byte("wheel"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("hosted upload = %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/simple/widget/", nil, accept)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("hosted project = %d %s", response.StatusCode, body)
	}
	var hosted struct {
		Meta     map[string]any `json:"meta"`
		Versions []string       `json:"versions"`
		Files    []struct {
			Hashes map[string]string `json:"hashes"`
			Size   int64             `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &hosted); err != nil || hosted.Meta["api-version"] != "1.1" || len(hosted.Files) != 1 || hosted.Files[0].Hashes["sha256"] == "" || hosted.Files[0].Size != 5 || len(hosted.Versions) != 1 || hosted.Versions[0] != "1.0" {
		t.Fatalf("hosted project shape = %s, %v", body, err)
	}
}
