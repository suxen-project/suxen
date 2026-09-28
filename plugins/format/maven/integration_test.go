package maven_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/server"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/maven"
)

const adminToken = "maven-integration-admin-token"

type fixture struct {
	handler *server.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithProxyTTL(t, 150*time.Millisecond)
}

func newFixtureWithProxyTTL(t *testing.T, proxyManifestTTL time.Duration) *fixture {
	t.Helper()
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	blobPath := filepath.Join(dataDirectory, "blobs")
	blobStore, err := blob.NewFS(blobPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		DataDir:           dataDirectory,
		BlobURL:           "fs://" + blobPath,
		BootstrapUser:     "admin",
		BootstrapPassword: "integration-password",
		BootstrapToken:    adminToken,
		MaxUploadBytes:    16 << 20,
		ProxyManifestTTL:  proxyManifestTTL,
		OutboundTimeout:   5 * time.Second,
		// The proxy upstream in these tests is a loopback httptest server,
		// which the egress guard blocks by default.
		OutboundCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := server.New(cfg, metadata, blobStore, logger)
	// Drain detached best-effort goroutines (e.g. async last-download updates)
	// before metadata.Close and t.TempDir removal, so their writes cannot race
	// cleanup ("directory not empty"). Registered after metadata.Close so it runs
	// first (LIFO).
	t.Cleanup(func() { _ = handler.Close() })
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{handler: handler}
}

func (f *fixture) do(
	t *testing.T,
	method string,
	requestPath string,
	body []byte,
	authenticated bool,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+adminToken)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func (f *fixture) createRepository(t *testing.T, spec map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		"/api/v1/repositories",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+adminToken)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func mustStatus(t *testing.T, response *http.Response, want int) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, want, body)
	}
	return body
}

func problemCode(t *testing.T, body []byte) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	return problem.Code
}

func TestHostedMavenRepositoryEnforcesVersionPolicy(t *testing.T) {
	f := newFixture(t)
	response := f.createRepository(t, map[string]any{
		"name":         "maven-releases",
		"format":       "maven",
		"type":         "hosted",
		"formatConfig": map[string]any{"versionPolicy": "release"},
	})
	mustStatus(t, response, http.StatusCreated)

	// A standard mvn deploy: artifact, checksum, then client-managed metadata.
	artifact := []byte("release artifact bytes")
	uploaded := f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/com/example/app/1.0/app-1.0.jar",
		artifact,
		true,
	)
	body := mustStatus(t, uploaded, http.StatusCreated)

	var asset struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal(body, &asset); err != nil {
		t.Fatal(err)
	}
	maven, _ := asset.Attributes["maven"].(map[string]any)
	if maven["groupId"] != "com.example" || maven["artifactId"] != "app" ||
		maven["version"] != "1.0" || maven["snapshot"] != false {
		t.Fatalf("projected maven attributes = %v", asset.Attributes)
	}

	checksum := f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/com/example/app/1.0/app-1.0.jar.sha1",
		[]byte("a94a8fe5ccb19ba61c4c0873d391e987982fbbd3"),
		true,
	)
	mustStatus(t, checksum, http.StatusCreated)
	metadata := f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/com/example/app/maven-metadata.xml",
		[]byte("<metadata/>"),
		true,
	)
	mustStatus(t, metadata, http.StatusCreated)

	downloaded := f.do(
		t,
		http.MethodGet,
		"/repository/maven-releases/com/example/app/1.0/app-1.0.jar",
		nil,
		true,
	)
	if got := mustStatus(t, downloaded, http.StatusOK); !bytes.Equal(got, artifact) {
		t.Fatalf("downloaded artifact = %q, want %q", got, artifact)
	}

	rejectedSnapshot := f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-1.jar",
		[]byte("snapshot bytes"),
		true,
	)
	if code := problemCode(t, mustStatus(t, rejectedSnapshot, http.StatusBadRequest)); code != "maven_version_policy" {
		t.Fatalf("snapshot rejection code = %q", code)
	}

	rejectedForeign := f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/random/file.bin",
		[]byte("junk"),
		true,
	)
	if code := problemCode(t, mustStatus(t, rejectedForeign, http.StatusBadRequest)); code != "maven_invalid_path" {
		t.Fatalf("foreign path rejection code = %q", code)
	}
}

func TestSnapshotMavenRepositoryAcceptsTimestampedDeploys(t *testing.T) {
	f := newFixture(t)
	response := f.createRepository(t, map[string]any{
		"name":         "maven-snapshots",
		"format":       "maven",
		"type":         "hosted",
		"formatConfig": map[string]any{"versionPolicy": "snapshot"},
	})
	mustStatus(t, response, http.StatusCreated)

	base := "/repository/maven-snapshots/com/example/app/1.0-SNAPSHOT/"
	uploaded := f.do(
		t,
		http.MethodPut,
		base+"app-1.0-20260807.120000-1.jar",
		[]byte("snapshot build one"),
		true,
	)
	body := mustStatus(t, uploaded, http.StatusCreated)
	var asset struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal(body, &asset); err != nil {
		t.Fatal(err)
	}
	maven, _ := asset.Attributes["maven"].(map[string]any)
	if maven["baseVersion"] != "1.0-SNAPSHOT" || maven["snapshot"] != true ||
		maven["version"] != "1.0-20260807.120000-1" {
		t.Fatalf("projected snapshot attributes = %v", asset.Attributes)
	}

	versionMetadata := f.do(
		t,
		http.MethodPut,
		base+"maven-metadata.xml",
		[]byte("<metadata/>"),
		true,
	)
	mustStatus(t, versionMetadata, http.StatusCreated)

	rejectedRelease := f.do(
		t,
		http.MethodPut,
		"/repository/maven-snapshots/com/example/app/1.0/app-1.0.jar",
		[]byte("release bytes"),
		true,
	)
	if code := problemCode(t, mustStatus(t, rejectedRelease, http.StatusBadRequest)); code != "maven_version_policy" {
		t.Fatalf("release rejection code = %q", code)
	}
}

func TestMavenFormatConfigValidation(t *testing.T) {
	f := newFixture(t)

	invalidPolicy := f.createRepository(t, map[string]any{
		"name":         "maven-invalid",
		"format":       "maven",
		"type":         "hosted",
		"formatConfig": map[string]any{"versionPolicy": "weekly"},
	})
	if code := problemCode(t, mustStatus(t, invalidPolicy, http.StatusBadRequest)); code != "invalid_format_config" {
		t.Fatalf("invalid policy code = %q", code)
	}

	unknownKey := f.createRepository(t, map[string]any{
		"name":         "maven-unknown",
		"format":       "maven",
		"type":         "hosted",
		"formatConfig": map[string]any{"retention": 30},
	})
	if code := problemCode(t, mustStatus(t, unknownKey, http.StatusBadRequest)); code != "invalid_format_config" {
		t.Fatalf("unknown key code = %q", code)
	}

	rawWithConfig := f.createRepository(t, map[string]any{
		"name":         "raw-with-config",
		"format":       "raw",
		"type":         "hosted",
		"formatConfig": map[string]any{"versionPolicy": "release"},
	})
	if code := problemCode(t, mustStatus(t, rawWithConfig, http.StatusBadRequest)); code != "invalid_format_config" {
		t.Fatalf("raw formatConfig code = %q", code)
	}
}

func TestMavenProxyCachesReleasesAndRevalidatesMetadata(t *testing.T) {
	// Leave enough headroom for two requests under the race detector. A very
	// short TTL makes the cache-hit assertion depend on CI runner load.
	const proxyManifestTTL = 2 * time.Second
	f := newFixtureWithProxyTTL(t, proxyManifestTTL)

	artifactCalls := atomic.Int64{}
	metadataCalls := atomic.Int64{}
	metadataVersion := atomic.Int64{}
	metadataVersion.Store(1)
	artifact := []byte("upstream release artifact")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/com/example/app/1.0/app-1.0.jar":
			artifactCalls.Add(1)
			_, _ = w.Write(artifact)
		case "/com/example/app/maven-metadata.xml":
			metadataCalls.Add(1)
			fmt.Fprintf(w, "<metadata><version>%d</version></metadata>", metadataVersion.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	response := f.createRepository(t, map[string]any{
		"name":     "maven-central",
		"format":   "maven",
		"type":     "proxy",
		"upstream": upstream.URL,
	})
	mustStatus(t, response, http.StatusCreated)

	// Release artifacts cache immutably: one upstream fetch serves both reads.
	for range 2 {
		downloaded := f.do(
			t,
			http.MethodGet,
			"/repository/maven-central/com/example/app/1.0/app-1.0.jar",
			nil,
			true,
		)
		if got := mustStatus(t, downloaded, http.StatusOK); !bytes.Equal(got, artifact) {
			t.Fatalf("proxied artifact = %q", got)
		}
	}
	if calls := artifactCalls.Load(); calls != 1 {
		t.Fatalf("artifact upstream calls = %d, want 1", calls)
	}

	// Metadata is mutable: cached within the TTL, revalidated after it.
	metadataPath := "/repository/maven-central/com/example/app/maven-metadata.xml"
	first := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Contains(first, []byte("<version>1</version>")) {
		t.Fatalf("first metadata = %s", first)
	}
	within := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Contains(within, []byte("<version>1</version>")) {
		t.Fatalf("cached metadata = %s", within)
	}
	if calls := metadataCalls.Load(); calls != 1 {
		t.Fatalf("metadata upstream calls before TTL = %d, want 1", calls)
	}

	metadataVersion.Store(2)
	time.Sleep(proxyManifestTTL + 250*time.Millisecond)
	refreshed := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Contains(refreshed, []byte("<version>2</version>")) {
		t.Fatalf("revalidated metadata = %s", refreshed)
	}
	if calls := metadataCalls.Load(); calls != 2 {
		t.Fatalf("metadata upstream calls after TTL = %d, want 2", calls)
	}
}

func TestMavenGroupServesMembers(t *testing.T) {
	f := newFixture(t)
	for _, spec := range []map[string]any{
		{"name": "maven-releases", "format": "maven", "type": "hosted",
			"formatConfig": map[string]any{"versionPolicy": "release"}},
		{"name": "maven-snapshots", "format": "maven", "type": "hosted",
			"formatConfig": map[string]any{"versionPolicy": "snapshot"}},
		{"name": "maven-all", "format": "maven", "type": "group",
			"members": []string{"maven-releases", "maven-snapshots"}},
	} {
		mustStatus(t, f.createRepository(t, spec), http.StatusCreated)
	}

	release := []byte("release via group")
	mustStatus(t, f.do(
		t,
		http.MethodPut,
		"/repository/maven-releases/com/example/app/1.0/app-1.0.jar",
		release,
		true,
	), http.StatusCreated)
	snapshot := []byte("snapshot via group")
	mustStatus(t, f.do(
		t,
		http.MethodPut,
		"/repository/maven-snapshots/com/example/app/1.1-SNAPSHOT/app-1.1-20260807.120000-1.jar",
		snapshot,
		true,
	), http.StatusCreated)

	fromGroup := f.do(
		t,
		http.MethodGet,
		"/repository/maven-all/com/example/app/1.0/app-1.0.jar",
		nil,
		true,
	)
	if got := mustStatus(t, fromGroup, http.StatusOK); !bytes.Equal(got, release) {
		t.Fatalf("group release = %q", got)
	}
	fromGroupSnapshot := f.do(
		t,
		http.MethodGet,
		"/repository/maven-all/com/example/app/1.1-SNAPSHOT/app-1.1-20260807.120000-1.jar",
		nil,
		true,
	)
	if got := mustStatus(t, fromGroupSnapshot, http.StatusOK); !bytes.Equal(got, snapshot) {
		t.Fatalf("group snapshot = %q", got)
	}
}

func TestMavenGroupMergesMetadata(t *testing.T) {
	f := newFixture(t)
	for _, spec := range []map[string]any{
		{"name": "maven-releases", "format": "maven", "type": "hosted",
			"formatConfig": map[string]any{"versionPolicy": "release"}},
		{"name": "maven-snapshots", "format": "maven", "type": "hosted",
			"formatConfig": map[string]any{"versionPolicy": "snapshot"}},
		{"name": "maven-all", "format": "maven", "type": "group",
			"members": []string{"maven-releases", "maven-snapshots"}},
	} {
		mustStatus(t, f.createRepository(t, spec), http.StatusCreated)
	}

	releaseMetadata := `<metadata>
  <groupId>com.example</groupId><artifactId>app</artifactId>
  <versioning>
    <latest>1.0</latest><release>1.0</release>
    <versions><version>1.0</version></versions>
    <lastUpdated>20260801000000</lastUpdated>
  </versioning>
</metadata>`
	snapshotMetadata := `<metadata>
  <groupId>com.example</groupId><artifactId>app</artifactId>
  <versioning>
    <latest>2.0-SNAPSHOT</latest>
    <versions><version>2.0-SNAPSHOT</version></versions>
    <lastUpdated>20260807000000</lastUpdated>
  </versioning>
</metadata>`
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/maven-releases/com/example/app/maven-metadata.xml",
		[]byte(releaseMetadata), true), http.StatusCreated)
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/maven-snapshots/com/example/app/maven-metadata.xml",
		[]byte(snapshotMetadata), true), http.StatusCreated)

	merged := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-all/com/example/app/maven-metadata.xml", nil, true),
		http.StatusOK)
	for _, needle := range []string{
		"<version>1.0</version>",
		"<version>2.0-SNAPSHOT</version>",
		"<latest>2.0-SNAPSHOT</latest>",
		"<release>1.0</release>",
		"<lastUpdated>20260807000000</lastUpdated>",
	} {
		if !bytes.Contains(merged, []byte(needle)) {
			t.Fatalf("merged metadata missing %s:\n%s", needle, merged)
		}
	}

	// The checksum companion must describe the merged document, not any
	// member's stored checksum.
	checksum := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-all/com/example/app/maven-metadata.xml.sha1", nil, true),
		http.StatusOK)
	expected := sha1.Sum(merged)
	if string(checksum) != hex.EncodeToString(expected[:]) {
		t.Fatalf("merged checksum = %s, want %s", checksum, hex.EncodeToString(expected[:]))
	}

	// Artifacts stay first-match through the same group.
	artifact := []byte("release artifact")
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/maven-releases/com/example/app/1.0/app-1.0.jar",
		artifact, true), http.StatusCreated)
	fromGroup := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-all/com/example/app/1.0/app-1.0.jar", nil, true),
		http.StatusOK)
	if !bytes.Equal(fromGroup, artifact) {
		t.Fatalf("group artifact = %q", fromGroup)
	}
}

func TestMavenHostedSynthesizesMetadata(t *testing.T) {
	f := newFixture(t)
	mustStatus(t, f.createRepository(t, map[string]any{
		"name": "maven-hosted", "format": "maven", "type": "hosted",
	}), http.StatusCreated)

	for _, assetPath := range []string{
		"com/example/app/1.0/app-1.0.jar",
		"com/example/app/1.1/app-1.1.jar",
		"com/example/app/2.0-SNAPSHOT/app-2.0-20260807.110000-2.jar",
		"com/example/app/2.0-SNAPSHOT/app-2.0-20260807.110000-2-sources.jar",
	} {
		mustStatus(t, f.do(t, http.MethodPut,
			"/repository/maven-hosted/"+assetPath,
			[]byte("content of "+assetPath), true), http.StatusCreated)
	}

	// No metadata was deployed: the artifact-level index is synthesized.
	synthesized := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/app/maven-metadata.xml", nil, true),
		http.StatusOK)
	for _, needle := range []string{
		"<version>1.0</version>",
		"<version>1.1</version>",
		"<version>2.0-SNAPSHOT</version>",
		"<latest>2.0-SNAPSHOT</latest>",
		"<release>1.1</release>",
	} {
		if !bytes.Contains(synthesized, []byte(needle)) {
			t.Fatalf("synthesized metadata missing %s:\n%s", needle, synthesized)
		}
	}
	// Synthesis must not depend on request time: checksum companions are
	// generated by a separate request and must describe the exact metadata
	// bytes even after the wall clock crosses a second boundary.
	nextSecond := time.Now().Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(nextSecond) + 50*time.Millisecond)
	checksum := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/app/maven-metadata.xml.sha256", nil, true),
		http.StatusOK)
	expected := sha256.Sum256(synthesized)
	if string(checksum) != hex.EncodeToString(expected[:]) {
		t.Fatalf("synthesized checksum = %s", checksum)
	}
	repeated := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/app/maven-metadata.xml", nil, true),
		http.StatusOK)
	if !bytes.Equal(repeated, synthesized) {
		t.Fatalf("repeated synthesized metadata changed:\nfirst:\n%s\nsecond:\n%s", synthesized, repeated)
	}

	// Version-level snapshot metadata derives the newest build.
	versionLevel := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/app/2.0-SNAPSHOT/maven-metadata.xml", nil, true),
		http.StatusOK)
	for _, needle := range []string{
		"<timestamp>20260807.110000</timestamp>",
		"<buildNumber>2</buildNumber>",
		"<value>2.0-20260807.110000-2</value>",
		"<classifier>sources</classifier>",
	} {
		if !bytes.Contains(versionLevel, []byte(needle)) {
			t.Fatalf("version metadata missing %s:\n%s", needle, versionLevel)
		}
	}

	// Client-deployed metadata wins over synthesis.
	stored := []byte("<metadata><groupId>com.example</groupId></metadata>")
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/maven-hosted/com/example/app/maven-metadata.xml",
		stored, true), http.StatusCreated)
	served := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/app/maven-metadata.xml", nil, true),
		http.StatusOK)
	if !bytes.Equal(served, stored) {
		t.Fatalf("stored metadata must win over synthesis, got:\n%s", served)
	}

	// Paths with no artifacts stay 404.
	response := f.do(t, http.MethodGet,
		"/repository/maven-hosted/com/example/other/maven-metadata.xml", nil, true)
	mustStatus(t, response, http.StatusNotFound)
}

func TestMavenHostedSynthesizesArtifactIDEndingInSnapshot(t *testing.T) {
	f := newFixture(t)
	mustStatus(t, f.createRepository(t, map[string]any{
		"name": "maven-hosted", "format": "maven", "type": "hosted",
	}), http.StatusCreated)
	artifact := "/repository/maven-hosted/com/example/widget-SNAPSHOT/1.0/widget-SNAPSHOT-1.0.jar"
	mustStatus(t, f.do(t, http.MethodPut, artifact, []byte("jar"), true), http.StatusCreated)
	metadataPath := "/repository/maven-hosted/com/example/widget-SNAPSHOT/maven-metadata.xml"
	metadata := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Contains(metadata, []byte("<artifactId>widget-SNAPSHOT</artifactId>")) ||
		!bytes.Contains(metadata, []byte("<version>1.0</version>")) {
		t.Fatalf("wrong artifact-level metadata: %s", metadata)
	}
	checksum := mustStatus(t, f.do(t, http.MethodGet, metadataPath+".sha256", nil, true), http.StatusOK)
	sum := sha256.Sum256(metadata)
	if string(checksum) != hex.EncodeToString(sum[:]) {
		t.Fatalf("synthesized metadata checksum = %q", checksum)
	}
	// The same metadata path can also index com:example:widget-SNAPSHOT.
	// When direct timestamped files exist, that version-level meaning wins.
	direct := "/repository/maven-hosted/com/example/widget-SNAPSHOT/example-widget-20260807.120000-1.jar"
	mustStatus(t, f.do(t, http.MethodPut, direct, []byte("snapshot"), true), http.StatusCreated)
	versionMetadata := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Contains(versionMetadata, []byte("<version>widget-SNAPSHOT</version>")) ||
		!bytes.Contains(versionMetadata, []byte("<value>widget-20260807.120000-1</value>")) {
		t.Fatalf("colliding path did not prefer direct snapshot artifacts: %s", versionMetadata)
	}
	stored := []byte("<metadata><groupId>operator-selected</groupId></metadata>")
	mustStatus(t, f.do(t, http.MethodPut, metadataPath, stored, true), http.StatusCreated)
	served := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
	if !bytes.Equal(served, stored) {
		t.Fatalf("stored metadata lost at colliding path: %s", served)
	}
}

func TestMavenGroupMergesSynthesizedMemberMetadata(t *testing.T) {
	f := newFixture(t)
	for _, spec := range []map[string]any{
		{"name": "team-a", "format": "maven", "type": "hosted"},
		{"name": "team-b", "format": "maven", "type": "hosted"},
		{"name": "team-all", "format": "maven", "type": "group",
			"members": []string{"team-a", "team-b"}},
	} {
		mustStatus(t, f.createRepository(t, spec), http.StatusCreated)
	}
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/team-a/com/example/lib/1.0/lib-1.0.jar",
		[]byte("a"), true), http.StatusCreated)
	mustStatus(t, f.do(t, http.MethodPut,
		"/repository/team-b/com/example/lib/1.1/lib-1.1.jar",
		[]byte("b"), true), http.StatusCreated)

	// Neither member has stored metadata; the group merges both members'
	// synthesized indexes.
	merged := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/team-all/com/example/lib/maven-metadata.xml", nil, true),
		http.StatusOK)
	if !bytes.Contains(merged, []byte("<version>1.0</version>")) ||
		!bytes.Contains(merged, []byte("<version>1.1</version>")) {
		t.Fatalf("merged synthesized metadata = %s", merged)
	}
}

func TestMavenSynthesizedMetadataWhenCoordinateContainsMetadataFilename(t *testing.T) {
	f := newFixture(t)
	for _, spec := range []map[string]any{
		{"name": "hosted", "format": "maven", "type": "hosted"},
		{"name": "group", "format": "maven", "type": "group", "members": []string{"hosted"}},
	} {
		mustStatus(t, f.createRepository(t, spec), http.StatusCreated)
	}
	assetPath := "com/example/maven-metadata.xml/1.0/maven-metadata.xml-1.0.jar"
	mustStatus(t, f.do(t, http.MethodPut, "/repository/hosted/"+assetPath,
		[]byte("jar bytes"), true), http.StatusCreated)

	for _, repository := range []string{"hosted", "group"} {
		metadataPath := "/repository/" + repository +
			"/com/example/maven-metadata.xml/maven-metadata.xml"
		body := mustStatus(t, f.do(t, http.MethodGet, metadataPath, nil, true), http.StatusOK)
		for _, expected := range []string{
			"<groupId>com.example</groupId>",
			"<artifactId>maven-metadata.xml</artifactId>",
			"<version>1.0</version>",
		} {
			if !bytes.Contains(body, []byte(expected)) {
				t.Fatalf("%s metadata missing %s: %s", repository, expected, body)
			}
		}
	}
}

func TestMavenProxyVersionPolicy(t *testing.T) {
	f := newFixture(t)
	requests := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		switch r.URL.Path {
		case "/com/example/app/1.0/app-1.0.jar":
			_, _ = w.Write([]byte("release content"))
		case "/.index/nexus-maven-repository-index.properties":
			_, _ = w.Write([]byte("index properties"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	mustStatus(t, f.createRepository(t, map[string]any{
		"name": "central-releases", "format": "maven", "type": "proxy",
		"upstream":     upstream.URL,
		"formatConfig": map[string]any{"versionPolicy": "release"},
	}), http.StatusCreated)

	// SNAPSHOT paths are refused before the upstream is consulted.
	rejected := f.do(t, http.MethodGet,
		"/repository/central-releases/com/example/app/1.0-SNAPSHOT/app-1.0-20260807.110000-1.jar",
		nil, true)
	if code := problemCode(t, mustStatus(t, rejected, http.StatusBadRequest)); code != "maven_version_policy" {
		t.Fatalf("proxy snapshot rejection code = %q", code)
	}
	select {
	case path := <-requests:
		t.Fatalf("upstream was consulted for a policy-rejected path: %s", path)
	default:
	}

	// Release artifacts and non-layout upstream files pass through.
	release := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/central-releases/com/example/app/1.0/app-1.0.jar", nil, true),
		http.StatusOK)
	if !bytes.Equal(release, []byte("release content")) {
		t.Fatalf("proxied release = %q", release)
	}
	index := mustStatus(t, f.do(t, http.MethodGet,
		"/repository/central-releases/.index/nexus-maven-repository-index.properties",
		nil, true), http.StatusOK)
	if !bytes.Equal(index, []byte("index properties")) {
		t.Fatalf("proxied index file = %q", index)
	}

	// A snapshot-policy proxy is the mirror image.
	mustStatus(t, f.createRepository(t, map[string]any{
		"name": "central-snapshots", "format": "maven", "type": "proxy",
		"upstream":     upstream.URL,
		"formatConfig": map[string]any{"versionPolicy": "snapshot"},
	}), http.StatusCreated)
	rejectedRelease := f.do(t, http.MethodGet,
		"/repository/central-snapshots/com/example/app/1.0/app-1.0.jar", nil, true)
	if code := problemCode(t, mustStatus(t, rejectedRelease, http.StatusBadRequest)); code != "maven_version_policy" {
		t.Fatalf("proxy release rejection code = %q", code)
	}
}
