package npm_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNpmHostedAllowOverwriteReplacesVersionAtomically(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted", "allowOverwrite": true}))
	first := tarball(t, "widget", "1.0.0")
	second := bytes.Clone(first)
	// A gzip modification time changes the package bytes while preserving a
	// valid tarball and its package.json identity.
	second[4] = 1
	publish := func(payload []byte) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"name": "widget",
			"versions": map[string]any{"1.0.0": map[string]any{
				"name": "widget", "version": "1.0.0",
				"dist": map[string]any{"integrity": sha512Integrity(payload)},
			}},
			"_attachments": map[string]any{"widget-1.0.0.tgz": map[string]any{
				"data": base64.StdEncoding.EncodeToString(payload),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		response, content := f.do(t, http.MethodPut, "/repository/hosted/widget", body, http.Header{"Content-Type": {"application/json"}})
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("publish: %d %s", response.StatusCode, content)
		}
	}
	publish(first)
	publish(second)
	response, content := f.do(t, http.MethodGet, "/repository/hosted/widget/-/widget-1.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(content, second) {
		t.Fatalf("stored tarball: %d (%d bytes)", response.StatusCode, len(content))
	}
	response, content = f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("packument: %d %s", response.StatusCode, content)
	}
	var packument struct {
		Versions map[string]struct {
			Dist struct {
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(content, &packument); err != nil {
		t.Fatal(err)
	}
	if got := packument.Versions["1.0.0"].Dist.Integrity; got != sha512Integrity(second) {
		t.Fatalf("packument integrity = %s", got)
	}
}
