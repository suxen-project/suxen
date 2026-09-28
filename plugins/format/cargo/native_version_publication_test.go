package cargo_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCargoPublishRespectsNativeCoreLimits(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	database, err := sql.Open("sqlite", filepath.Join(f.dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, tc := range []struct {
		version string
		status  int
	}{
		{"18446744073709551616.0.0", http.StatusBadRequest},
		{"0.18446744073709551616.0", http.StatusBadRequest},
		{"0.0.18446744073709551616", http.StatusBadRequest},
		{"18446744073709551615.0.0", http.StatusOK},
		{"1.0.0-18446744073709551616", http.StatusOK},
	} {
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, tc.version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte("archive")), nil)
		if response.StatusCode != tc.status {
			t.Errorf("publish %q = %d: %s; want %d", tc.version, response.StatusCode, body, tc.status)
		}
		if tc.status == http.StatusBadRequest {
			var stored int
			identity, _, _ := strings.Cut(tc.version, "+")
			err := database.QueryRow(`SELECT COUNT(*) FROM assets WHERE path IN (?, ?, ?)`,
				"dl/widget/"+tc.version+"/download", "index-meta/widget/"+tc.version+".json",
				"index-claim/widget/"+identity+".txt").Scan(&stored)
			if err != nil || stored != 0 {
				t.Fatalf("rejected %q left %d crate/index/claim assets: %v", tc.version, stored, err)
			}
		}
	}
	response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || strings.Count(string(body), "\n") != 2 ||
		strings.Contains(string(body), "18446744073709551616.0.0") ||
		!strings.Contains(string(body), "18446744073709551615.0.0") ||
		!strings.Contains(string(body), "1.0.0-18446744073709551616") {
		t.Fatalf("hosted index = %d: %s", response.StatusCode, body)
	}
}
