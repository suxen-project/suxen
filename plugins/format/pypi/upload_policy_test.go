package pypi_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestPypiHostedAllowOverwriteReuploadUpdatesIndexAndDownload(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "replaceable", "format": "pypi", "type": "hosted", "allowOverwrite": true,
	}))
	filename := "hello-1.0.0-py3-none-any.whl"
	for _, payload := range []string{"first distribution", "replacement distribution"} {
		response, body := twineUpload(t, f, "replaceable", "hello", "1.0.0", filename, []byte(payload))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("native upload %q = %d %s", payload, response.StatusCode, body)
		}
		response, body = f.do(t, http.MethodGet, "/repository/replaceable/simple/hello/", nil,
			http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("project index = %d %s", response.StatusCode, body)
		}
		var page struct {
			Files []struct {
				Filename string            `json:"filename"`
				Hashes   map[string]string `json:"hashes"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
		if len(page.Files) != 1 || page.Files[0].Filename != filename || page.Files[0].Hashes["sha256"] != wantDigest {
			t.Fatalf("index after %q = %s, want SHA-256 %s", payload, body, wantDigest)
		}
		response, body = f.do(t, http.MethodGet, "/repository/replaceable/packages/hello/"+filename, nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != payload {
			t.Fatalf("download after %q = %d %s", payload, response.StatusCode, body)
		}
	}
}

func TestPypiHostedRejectsGenericUploads(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	writer := f.writeOnlyToken(t)
	payload := wheel(t, "hello", "1.0.0")
	filename := "hello-1.0.0-py3-none-any.whl"
	response, body := twineUpload(t, f, "hosted", "hello", "1.0.0", filename, payload)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("native upload: %d %s", response.StatusCode, body)
	}
	headers := http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}}
	response, before := f.do(t, http.MethodGet, "/repository/hosted/simple/hello/", nil, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("project page: %d %s", response.StatusCode, before)
	}
	f.rejectGenericUploads(t, writer, []string{
		"packages/hello/" + filename, "simple/hello/", "simple/",
	}, []string{
		"packages/hello/hello-2.0.0-py3-none-any.whl", "arbitrary/path",
	}, "pypi_native_publish_required")
	response, after := f.do(t, http.MethodGet, "/repository/hosted/simple/hello/", nil, headers)
	if response.StatusCode != http.StatusOK || !bytes.Equal(after, before) {
		t.Fatalf("generic uploads changed project page: %d %s", response.StatusCode, after)
	}
}

func (f *fixture) writeOnlyToken(t *testing.T) string {
	t.Helper()
	response, body := f.do(t, http.MethodPost, "/api/v1/users/admin/tokens",
		[]byte(`{"name":"publisher","scopes":["repository:hosted:write"]}`),
		http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create write-only token: %d %s", response.StatusCode, body)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.Token == "" {
		t.Fatalf("decode write-only token: %v", err)
	}
	return token.Token
}

// Generic writes must neither replace published assets nor inject new assets
// outside the native publication transaction, even for an administrator.
func (f *fixture) rejectGenericUploads(t *testing.T, writer string, existing, absent []string, code string) {
	t.Helper()
	for _, assetPath := range existing {
		response, before := f.do(t, http.MethodGet, "/repository/hosted/"+assetPath, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("read original %s: %d %s", assetPath, response.StatusCode, before)
		}
		for _, token := range []string{writer, adminToken} {
			response, body := f.do(t, http.MethodPut, "/repository/hosted/"+assetPath,
				[]byte("replacement"), http.Header{"Authorization": {"Bearer " + token}})
			if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(code)) {
				t.Fatalf("generic overwrite %s: %d %s", assetPath, response.StatusCode, body)
			}
		}
		response, after := f.do(t, http.MethodGet, "/repository/hosted/"+assetPath, nil, nil)
		if response.StatusCode != http.StatusOK || !bytes.Equal(after, before) {
			t.Fatalf("generic overwrite changed %s: %d %s", assetPath, response.StatusCode, after)
		}
	}
	for _, assetPath := range absent {
		response, body := f.do(t, http.MethodPut, "/repository/hosted/"+assetPath,
			[]byte("injected"), http.Header{"Authorization": {"Bearer " + writer}})
		if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(code)) {
			t.Fatalf("generic creation %s: %d %s", assetPath, response.StatusCode, body)
		}
		response, body = f.do(t, http.MethodGet, "/repository/hosted/"+assetPath, nil, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("generic creation left asset %s: %d %s", assetPath, response.StatusCode, body)
		}
	}
	// Establish that the token used above cannot delete published content.
	response, body := f.do(t, http.MethodDelete, "/repository/hosted/"+existing[0],
		nil, http.Header{"Authorization": {"Bearer " + writer}})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("write-only delete: %d %s", response.StatusCode, body)
	}
}
