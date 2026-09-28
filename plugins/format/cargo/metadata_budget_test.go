package cargo_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
)

const cargoMetadataInputLimit = 1 << 20

func TestCargoExpandedMetadataRoundTrip(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))

	// The request fits the advertised metadata frame, but the checksum and
	// yanked fields make its persisted index entry larger than that frame.
	prefix := `{"name":"widget","vers":"1.0.0","deps":[],"features":{"`
	suffix := `":[]}}`
	feature := strings.Repeat("a", cargoMetadataInputLimit-32-len(prefix)-len(suffix))
	metadata := prefix + feature + suffix
	if len(metadata) != cargoMetadataInputLimit-32 {
		t.Fatalf("test metadata size = %d", len(metadata))
	}
	crate := crateFile(t, "widget", "1.0.0")
	publish := func() {
		t.Helper()
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, metadata, crate), nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("publish near input limit: %d %s", response.StatusCode, body)
		}
	}
	publish()
	response, stored := f.do(t, http.MethodGet, "/repository/hosted/index-meta/widget/1.0.0.json", nil, nil)
	if response.StatusCode != http.StatusOK || len(stored) <= cargoMetadataInputLimit {
		t.Fatalf("persisted expanded entry: status=%d size=%d", response.StatusCode, len(stored))
	}
	response, index := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(index, append(bytes.Clone(stored), '\n')) {
		t.Fatalf("index did not round-trip stored entry: status=%d size=%d", response.StatusCode, len(index))
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"hosted"}}))
	response, grouped := f.do(t, http.MethodGet, "/repository/group/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(grouped, index) {
		t.Fatalf("group index did not round-trip entry: status=%d size=%d", response.StatusCode, len(grouped))
	}
	response, groupedCrate := f.do(t, http.MethodGet, "/repository/group/dl/widget/1.0.0/download", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(groupedCrate, crate) {
		t.Fatalf("group crate fetch: status=%d size=%d", response.StatusCode, len(groupedCrate))
	}
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "proxy", "format": "cargo", "type": "proxy",
		"upstream": f.suxen.URL + "/repository/hosted",
	}))
	response, config := f.do(t, http.MethodGet, "/repository/proxy/config.json", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxy config: status=%d body=%s", response.StatusCode, config)
	}
	response, proxied := f.do(t, http.MethodGet, "/repository/proxy/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(proxied, index) {
		t.Fatalf("proxy index did not round-trip entry: status=%d size=%d", response.StatusCode, len(proxied))
	}
	response, proxiedCrate := f.do(t, http.MethodGet, "/repository/proxy/dl/widget/1.0.0/download", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(proxiedCrate, crate) {
		t.Fatalf("proxy crate fetch: status=%d size=%d body=%s", response.StatusCode, len(proxiedCrate), proxiedCrate)
	}
	publish()

	// Older repositories have only the canonical metadata row as the version
	// claim. Its larger entry must still be readable on an identical retry.
	database := cargoMetadataDB(t, f)
	result, err := database.Exec(`DELETE FROM assets WHERE path = ?`, "index-claim/widget/1.0.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := result.RowsAffected(); err != nil || deleted != 1 {
		t.Fatalf("remove new-style claim: rows=%d err=%v", deleted, err)
	}
	publish()
}

func TestCargoDependencyNormalizationCanExpandPastInputLimit(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	const dependencyCount = 12000
	metadata := `{"name":"widget","vers":"1.0.0","deps":[` +
		strings.TrimSuffix(strings.Repeat(`{"name":"dep","version_req":"*"},`, dependencyCount), ",") +
		`],"features":{}}`
	if len(metadata) >= cargoMetadataInputLimit {
		t.Fatalf("dependency request exceeds input limit: %d", len(metadata))
	}
	response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new",
		cargoPublishBody(t, metadata, crateFile(t, "widget", "1.0.0")), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("publish dependencies: %d %s", response.StatusCode, body)
	}
	response, index := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || len(index) <= cargoMetadataInputLimit {
		t.Fatalf("expanded dependency index: status=%d size=%d", response.StatusCode, len(index))
	}
	var entry struct {
		Deps []struct {
			Name     string   `json:"name"`
			Req      string   `json:"req"`
			Features []string `json:"features"`
			Kind     string   `json:"kind"`
		} `json:"deps"`
	}
	if err := json.Unmarshal(bytes.TrimSuffix(index, []byte{'\n'}), &entry); err != nil {
		t.Fatalf("decode expanded index: %v", err)
	}
	if len(entry.Deps) != dependencyCount || entry.Deps[0].Name != "dep" || entry.Deps[0].Req != "*" || entry.Deps[0].Kind != "normal" || entry.Deps[0].Features == nil {
		t.Fatalf("normalized dependencies: count=%d first=%+v", len(entry.Deps), entry.Deps[0])
	}
}

func TestCargoRejectsOversizedGeneratedEntryAtomically(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted", "allowOverwrite": true}))
	// Empty input dependencies acquire their full index representation. This
	// stays within the input frame while exceeding the persisted entry budget.
	metadata := `{"name":"widget","vers":"1.0.0","deps":[` +
		strings.TrimSuffix(strings.Repeat(`{},`, 85000), ",") + `],"features":{}}`
	if len(metadata) >= cargoMetadataInputLimit {
		t.Fatalf("oversize test input exceeds frame: %d", len(metadata))
	}
	response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new",
		cargoPublishBody(t, metadata, crateFile(t, "widget", "1.0.0")), nil)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized generated entry accepted: %d %s", response.StatusCode, body)
	}
	database := cargoMetadataDB(t, f)
	var stored int
	if err := database.QueryRow(`SELECT COUNT(*) FROM assets WHERE path IN (?, ?, ?)`,
		"dl/widget/1.0.0/download", "index-meta/widget/1.0.0.json", "index-claim/widget/1.0.0.txt").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("rejected publish left %d crate/index/claim assets: %v", stored, err)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected publish appears in index: %d %s", response.StatusCode, body)
	}
	corrected := `{"name":"widget","vers":"1.0.0","deps":[],"features":{}}`
	crate := crateFile(t, "widget", "1.0.0")
	response, body = f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, corrected, crate), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("corrected retry after oversize rejection: %d %s", response.StatusCode, body)
	}
	response, originalIndex := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("corrected index: %d %s", response.StatusCode, originalIndex)
	}
	response, body = f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new",
		cargoPublishBody(t, metadata, []byte("replacement")), nil)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized overwrite accepted: %d %s", response.StatusCode, body)
	}
	response, preservedIndex := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(preservedIndex, originalIndex) {
		t.Fatalf("rejected overwrite changed index: status=%d size=%d", response.StatusCode, len(preservedIndex))
	}
	response, preservedCrate := f.do(t, http.MethodGet, "/repository/hosted/dl/widget/1.0.0/download", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(preservedCrate, crate) {
		t.Fatalf("rejected overwrite changed crate: status=%d size=%d", response.StatusCode, len(preservedCrate))
	}
}

func TestCargoOversizedStoredEntryFailsWholeIndex(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	for _, version := range []string{"1.0.0", "2.0.0"} {
		metadata := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new",
			cargoPublishBody(t, metadata, crateFile(t, "widget", version)), nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("seed %s: %d %s", version, response.StatusCode, body)
		}
	}
	response, valid := f.do(t, http.MethodGet, "/repository/hosted/index-meta/widget/2.0.0.json", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read seeded metadata: %d %s", response.StatusCode, valid)
	}
	// Simulate a legacy persisted entry at the exact supported byte limit,
	// then one byte over it. A malformed oversized blob also must fail the
	// whole index rather than returning only its healthy sibling.
	storage, err := blob.NewFS(filepath.Join(f.dataDirectory, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	database := cargoMetadataDB(t, f)
	for _, test := range []struct {
		name      string
		size      int
		malformed bool
		status    int
	}{
		{"at supported limit", (8 << 20) - 1, false, http.StatusOK},
		{"one byte over", 8 << 20, false, http.StatusInternalServerError},
		{"malformed and oversized", (8 << 20) + 64, true, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := make([]byte, test.size)
			copy(stored, valid)
			for i := len(valid); i < len(stored); i++ {
				stored[i] = ' '
			}
			if test.malformed {
				stored[len(stored)-1] = '!'
			}
			sum := sha256.Sum256(stored)
			digest := fmt.Sprintf("sha256:%x", sum)
			if _, err := storage.Put(context.Background(), digest, bytes.NewReader(stored)); err != nil {
				t.Fatal(err)
			}
			result, err := database.Exec(`UPDATE assets SET digest = ?, size = ? WHERE path = ?`, digest, len(stored), "index-meta/widget/2.0.0.json")
			if err != nil {
				t.Fatal(err)
			}
			if updated, err := result.RowsAffected(); err != nil || updated != 1 {
				t.Fatalf("replace stored entry: rows=%d err=%v", updated, err)
			}
			response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
			if response.StatusCode != test.status {
				t.Fatalf("stored metadata size %d: index status=%d bytes=%d, want %d", test.size, response.StatusCode, len(body), test.status)
			}
			if test.status == http.StatusOK && (!bytes.Contains(body, []byte(`"vers":"1.0.0"`)) || !bytes.Contains(body, []byte(`"vers":"2.0.0"`))) {
				t.Fatalf("accepted boundary entry omitted a version: size=%d", len(body))
			}
		})
	}
}

func cargoMetadataDB(t *testing.T, f *fixture) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(f.dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
