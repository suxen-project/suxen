package npm_test

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNpmHostedRejectsGenericUploads(t *testing.T) {
	for _, name := range []string{"widget", "@acme/widget"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
			writer := f.writeOnlyToken(t)
			payload := tarball(t, name, "1.0.0")
			sum := sha512.Sum512(payload)
			publishBody, err := json.Marshal(map[string]any{
				"name": name,
				"versions": map[string]any{"1.0.0": map[string]any{
					"name": name, "version": "1.0.0",
					"dist": map[string]any{"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum[:])},
				}},
				"_attachments": map[string]any{"widget-1.0.0.tgz": map[string]any{
					"data": base64.StdEncoding.EncodeToString(payload),
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{"Authorization": {"Bearer " + writer}, "Content-Type": {"application/json"}}
			response, content := f.do(t, http.MethodPut, "/repository/hosted/"+name, publishBody, headers)
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("native publish with write-only token: %d %s", response.StatusCode, content)
			}
			response, before := f.do(t, http.MethodGet, "/repository/hosted/"+name, nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("packument: %d %s", response.StatusCode, before)
			}
			f.rejectGenericUploads(t, writer, []string{
				name + "/-/widget-1.0.0.tgz", name + "/-/metadata/1.0.0.json",
			}, []string{
				name + "/-/widget-2.0.0.tgz", name + "/-/metadata/2.0.0.json", "arbitrary/path",
			}, "npm_native_publish_required")
			response, after := f.do(t, http.MethodGet, "/repository/hosted/"+name, nil, nil)
			if response.StatusCode != http.StatusOK || !bytes.Equal(after, before) {
				t.Fatalf("generic uploads changed packument: %d %s", response.StatusCode, after)
			}
		})
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
