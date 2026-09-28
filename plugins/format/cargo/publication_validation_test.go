package cargo_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCargoRepublishConflictPreservesCrateAndIndex(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	requestPath := "/repository/hosted/api/v1/crates/new"
	meta := `{"name":"widget","vers":"1.0.0","deps":[],"features":{}}`
	first, body := f.do(t, http.MethodPut, requestPath, cargoPublishBody(t, meta, []byte("original crate")), nil)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("initial publish: %d %s", first.StatusCode, body)
	}
	index, originalIndex := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if index.StatusCode != http.StatusOK {
		t.Fatalf("original index: %d %s", index.StatusCode, originalIndex)
	}
	second, body := f.do(t, http.MethodPut, requestPath, cargoPublishBody(t, meta, []byte("changed crate")), nil)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("immutable conflict: %d %s", second.StatusCode, body)
	}
	idempotent, body := f.do(t, http.MethodPut, requestPath, cargoPublishBody(t, meta, []byte("original crate")), nil)
	if idempotent.StatusCode != http.StatusOK {
		t.Fatalf("byte-identical republish: %d %s", idempotent.StatusCode, body)
	}
	artifact, original := f.do(t, http.MethodGet, "/repository/hosted/dl/widget/1.0.0/download", nil, nil)
	if artifact.StatusCode != http.StatusOK || string(original) != "original crate" {
		t.Fatalf("rejected publication changed crate: %d %s", artifact.StatusCode, original)
	}
	index, after := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if index.StatusCode != http.StatusOK || !bytes.Equal(after, originalIndex) {
		t.Fatalf("rejected publication changed index: %d %s", index.StatusCode, after)
	}
}

func TestCargoMetadataChangeWithSameCrateConflictsByDefault(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	path := "/repository/hosted/api/v1/crates/new"
	crate := []byte("same crate")
	firstMeta := `{"name":"widget","vers":"1.0.0","deps":[],"features":{}}`
	changedMeta := `{"name":"widget","vers":"1.0.0","deps":[],"features":{"extra":[]}}`
	first, body := f.do(t, http.MethodPut, path, cargoPublishBody(t, firstMeta, crate), nil)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("initial publish: %d %s", first.StatusCode, body)
	}
	response, originalIndex := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial index: %d %s", response.StatusCode, originalIndex)
	}
	changed, body := f.do(t, http.MethodPut, path, cargoPublishBody(t, changedMeta, crate), nil)
	if changed.StatusCode != http.StatusConflict {
		t.Fatalf("metadata-only replacement: %d %s", changed.StatusCode, body)
	}
	response, after := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(after, originalIndex) {
		t.Fatalf("index changed after rejected metadata: %d %s", response.StatusCode, after)
	}
}

func TestCargoBuildMetadataCannotRepublish(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	publish := func(version, content string) int {
		t.Helper()
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(content)), nil)
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusConflict {
			t.Fatalf("publish %s: %d %s", version, response.StatusCode, body)
		}
		return response.StatusCode
	}
	if got := publish("1.0.0", "original"); got != http.StatusOK {
		t.Fatalf("first publish = %d", got)
	}
	if got := publish("1.0.0+different", "changed"); got != http.StatusConflict {
		t.Fatalf("build metadata republish = %d", got)
	}
	response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 1 || !bytes.Contains(body, []byte(`"vers":"1.0.0"`)) {
		t.Fatalf("index after conflict: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/dl/widget/1.0.0+different/download", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected crate exists: %d %s", response.StatusCode, body)
	}

	if got := publish("2.0.0+build.1", "build one"); got != http.StatusOK {
		t.Fatalf("build version publish = %d", got)
	}
	if got := publish("2.0.0+build.1", "build one"); got != http.StatusOK {
		t.Fatalf("identical retry = %d", got)
	}
	if got := publish("2.0.0+build.2", "build two"); got != http.StatusConflict {
		t.Fatalf("second build variant = %d", got)
	}
	if got := publish("2.0.0", "base"); got != http.StatusConflict {
		t.Fatalf("base version after build = %d", got)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 2 || !bytes.Contains(body, []byte(`"vers":"2.0.0+build.1"`)) {
		t.Fatalf("index with build version: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodDelete, "/repository/hosted/dl/widget/2.0.0+build.1/download", nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete build version: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/index-meta/widget/2.0.0.json", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("canonical claim remained: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 1 || bytes.Contains(body, []byte(`"vers":"2.0.0+build.1"`)) {
		t.Fatalf("deleted build version remained in index: %d %s", response.StatusCode, body)
	}
	if got := publish("2.0.0", "base"); got != http.StatusOK {
		t.Fatalf("publish after delete = %d", got)
	}
}

func TestCargoBuildMetadataClaimSurvivesOverwritePolicy(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted", "allowOverwrite": true}))
	for i, version := range []string{"1.0.0+one", "1.0.0+two"} {
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(version)), nil)
		want := http.StatusOK
		if i == 1 {
			want = http.StatusConflict
		}
		if response.StatusCode != want {
			t.Fatalf("publish %s = %d %s; want %d", version, response.StatusCode, body, want)
		}
	}
	replacementMeta := `{"name":"widget","vers":"1.0.0+one","deps":[],"features":{}}`
	replacement := []byte("replacement crate")
	response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, replacementMeta, replacement), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("exact version replacement: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/dl/widget/1.0.0+one/download", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, replacement) {
		t.Fatalf("replaced crate: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 1 || !bytes.Contains(body, []byte(`"vers":"1.0.0+one"`)) ||
		!bytes.Contains(body, []byte(fmt.Sprintf(`"cksum":"%x"`, sha256.Sum256(replacement)))) {
		t.Fatalf("index after conflicting build variant: %d %s", response.StatusCode, body)
	}
}

func TestCargoCaseVariantCannotShareVersionClaim(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted", "allowOverwrite": true}))
	for i, name := range []string{"Widget", "widget"} {
		meta := fmt.Sprintf(`{"name":%q,"vers":"1.0.0","deps":[],"features":{}}`, name)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(name)), nil)
		want := http.StatusOK
		if i == 1 {
			want = http.StatusConflict
		}
		if response.StatusCode != want {
			t.Fatalf("publish %s = %d %s; want %d", name, response.StatusCode, body, want)
		}
	}
	response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 1 || !bytes.Contains(body, []byte(`"name":"Widget"`)) {
		t.Fatalf("case-variant index: %d %s", response.StatusCode, body)
	}
}

func TestCargoLegacyBuildVersionClaimUpgrade(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted", "allowOverwrite": true}))
	publish := func(version, content string) (int, []byte) {
		t.Helper()
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(content)), nil)
		return response.StatusCode, body
	}
	if status, body := publish("1.0.0+one", "original"); status != http.StatusOK {
		t.Fatalf("initial publish: %d %s", status, body)
	}
	// Seed the old layout from a real published entry: a canonical index-meta
	// alias held the claim, and there was no separate index-claim row.
	database, err := sql.Open("sqlite", filepath.Join(f.dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Exec(`INSERT INTO assets
		(repository_id,path,digest,size,blob_store,content_type,kind,reference,subject_digest,attributes,created_at,updated_at,validated_at,last_accessed)
		SELECT repository_id, ?, digest,size,blob_store,content_type,kind,reference,subject_digest,attributes,created_at,updated_at,validated_at,last_accessed
		FROM assets WHERE path = ?`, "index-meta/widget/1.0.0.json", "index-meta/widget/1.0.0+one.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM assets WHERE path = ?`, "index-claim/widget/1.0.0.txt"); err != nil {
		t.Fatal(err)
	}
	if status, body := publish("1.0.0+two", "different"); status != http.StatusConflict {
		t.Fatalf("legacy claim allowed alternate spelling: %d %s", status, body)
	}
	if status, body := publish("1.0.0+one", "replacement"); status != http.StatusOK {
		t.Fatalf("legacy exact spelling replacement: %d %s", status, body)
	}
	response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || bytes.Count(body, []byte{'\n'}) != 1 ||
		!bytes.Contains(body, []byte(fmt.Sprintf(`"cksum":"%x"`, sha256.Sum256([]byte("replacement"))))) {
		t.Fatalf("index after legacy replacement: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodDelete, "/repository/hosted/dl/widget/1.0.0+one/download", nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete upgraded crate: %d %s", response.StatusCode, body)
	}
	for _, path := range []string{"index-meta/widget/1.0.0+one.json", "index-meta/widget/1.0.0.json", "index-claim/widget/1.0.0.txt"} {
		response, body = f.do(t, http.MethodGet, "/repository/hosted/"+path, nil, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("orphaned %s after delete: %d %s", path, response.StatusCode, body)
		}
	}
}

func TestCargoConcurrentBuildMetadataCollision(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	versions := []string{"3.0.0+one", "3.0.0+two"}
	statuses := make([]int, len(versions))
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i, version := range versions {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
			response, _ := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(version)), nil)
			statuses[i] = response.StatusCode
		}()
	}
	close(start)
	workers.Wait()
	if !(statuses[0] == http.StatusOK && statuses[1] == http.StatusConflict || statuses[1] == http.StatusOK && statuses[0] == http.StatusConflict) {
		t.Fatalf("concurrent publication statuses = %v", statuses)
	}
	response, body := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || strings.Count(string(body), "\n") != 1 {
		t.Fatalf("concurrent publication index: %d %s", response.StatusCode, body)
	}
}

func TestCargoRejectsInvalidVersions(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	for _, version := range []string{"", "1", "1.2", "01.2.3", "1.02.3", "1.2.03", "v1.2.3", "1.2.3+", "1.2.3+bad!", "1.2.3-01", "1.2.3/other"} {
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte("crate")), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid %q: %d %s", version, response.StatusCode, body)
		}
	}
	for _, version := range []string{"0.0.0", "1.2.3-alpha.1", "1.2.3+001", "1.2.4-rc.1+build.2"} {
		meta := fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version)
		response, body := f.do(t, http.MethodPut, "/repository/hosted/api/v1/crates/new", cargoPublishBody(t, meta, []byte(version)), nil)
		if response.StatusCode != http.StatusOK {
			t.Errorf("valid %q: %d %s", version, response.StatusCode, body)
		}
	}
}
