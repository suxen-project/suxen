package npm_test

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNpmPublishRespectsNativeVersionLimits(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
	database, err := sql.Open("sqlite", filepath.Join(f.dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, tc := range []struct {
		version string
		status  int
	}{
		{"9007199254740992.0.0", http.StatusBadRequest},
		{"1.0.0+" + strings.Repeat("a", 251), http.StatusBadRequest},
		{"9007199254740991.0.0", http.StatusCreated},
		{"1.0.0-" + strings.Repeat("9", 40), http.StatusCreated},
	} {
		body, err := json.Marshal(map[string]any{
			"name": "widget",
			"versions": map[string]any{tc.version: map[string]any{
				"name": "widget", "version": tc.version, "dist": map[string]any{},
			}},
			"_attachments": map[string]any{"widget-" + tc.version + ".tgz": map[string]any{
				"data": base64.StdEncoding.EncodeToString([]byte("archive")),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		response, payload := f.do(t, http.MethodPut, "/repository/hosted/widget", body,
			http.Header{"Content-Type": {"application/json"}})
		if response.StatusCode != tc.status {
			t.Errorf("publish %q = %d: %s; want %d", tc.version, response.StatusCode, payload, tc.status)
		}
		if tc.status == http.StatusBadRequest {
			var stored int
			err := database.QueryRow(`SELECT COUNT(*) FROM assets WHERE path IN (?, ?)`,
				"widget/-/metadata/"+tc.version+".json", "widget/-/widget-"+tc.version+".tgz").Scan(&stored)
			if err != nil || stored != 0 {
				t.Fatalf("rejected %q left %d metadata/tarball assets: %v", tc.version, stored, err)
			}
		}
	}
	response, payload := f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), `"latest":"9007199254740991.0.0"`) {
		t.Fatalf("hosted latest = %d: %s", response.StatusCode, payload)
	}
}
