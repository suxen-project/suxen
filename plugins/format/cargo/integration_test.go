package cargo_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/server"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/cargo"
)

const adminToken = "cargo-integration-admin-token"

type fixture struct {
	handler       *server.Server
	suxen         *httptest.Server
	dataDirectory string
}

func newFixture(t *testing.T, proxyTTL time.Duration) *fixture {
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
		ProxyManifestTTL:  proxyTTL,
		OutboundTimeout:   5 * time.Second,
		// The upstreams are loopback httptest servers, which the egress guard
		// blocks by default.
		OutboundCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := server.New(cfg, metadata, blobStore, logger)
	t.Cleanup(func() { _ = handler.Close() })
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpdateRole(context.Background(), domain.Role{
		Name:        "anonymous",
		Description: "Protocol test fixture read access",
		Privileges:  []string{"repository:*:read"},
	}); err != nil {
		t.Fatal(err)
	}
	suxen := httptest.NewServer(handler)
	t.Cleanup(suxen.Close)
	return &fixture{handler: handler, suxen: suxen, dataDirectory: dataDirectory}
}

func (f *fixture) do(t *testing.T, method, requestPath string, body []byte, header http.Header) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, f.suxen.URL+requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if host := header.Get("Host"); host != "" {
		request.Host = host
	}
	if request.Header.Get("Authorization") == "" {
		request.Header.Set("Authorization", "Bearer "+adminToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, content
}

func (f *fixture) createRepository(t *testing.T, spec map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	response, content := f.do(t, http.MethodPost, "/api/v1/repositories", body, http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("create repository %v: %d %s", spec["name"], response.StatusCode, content)
	}
	return response
}

func mustCreate(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create repository: status %d", response.StatusCode)
	}
}

// crateFile builds a minimal valid .crate (tar.gz with <name>-<version>/…).
func crateFile(t *testing.T, name, version string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	prefix := name + "-" + version + "/"
	files := map[string]string{
		"Cargo.toml": "[package]\nname = \"" + name + "\"\nversion = \"" + version + "\"\nedition = \"2021\"\n\n[lib]\npath = \"src/lib.rs\"\n",
		"src/lib.rs": "pub const VERSION: &str = \"" + version + "\";\n",
	}
	for path, content := range files {
		header := &tar.Header{Name: prefix + path, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(0, 0)}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// registry is a fake sparse registry: an index host and a separate download
// host, as crates.io has.
type registry struct {
	index    *httptest.Server
	download *httptest.Server
	hits     atomic.Int64
	blocked  atomic.Bool
	crates   map[string]map[string][]byte // name -> version -> crate bytes
}

func newRegistry(t *testing.T, crates map[string][]string) *registry {
	t.Helper()
	r := &registry{crates: map[string]map[string][]byte{}}
	for name, versions := range crates {
		r.crates[name] = map[string][]byte{}
		for _, version := range versions {
			r.crates[name][version] = crateFile(t, name, version)
		}
	}
	r.download = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if r.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		// /crates/<name>/<version>/download
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/crates/"), "/")
		if len(parts) != 3 || parts[2] != "download" {
			http.NotFound(w, req)
			return
		}
		content, found := r.crates[parts[0]][parts[1]]
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(r.download.Close)
	r.index = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if r.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		if req.URL.Path == "/config.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"dl":"`+r.download.URL+`/crates","api":"https://crates.example"}`)
			return
		}
		name := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		versions, found := r.crates[name]
		if !found {
			http.NotFound(w, req)
			return
		}
		for version, content := range versions {
			sum := sha256.Sum256(content)
			_, _ = io.WriteString(w, `{"name":"`+name+`","vers":"`+version+`","deps":[],"cksum":"`+hex.EncodeToString(sum[:])+`","features":{},"yanked":false}`+"\n")
		}
	}))
	t.Cleanup(r.index.Close)
	return r
}

func requireCargo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo binary not available")
	}
}

// newProject writes a crate depending on hello from the suxen registry.
func newProject(t *testing.T, f *fixture, repository string) string {
	t.Helper()
	directory := t.TempDir()
	files := map[string]string{
		"Cargo.toml":         "[package]\nname = \"consumer\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nhello = { version = \"0.1\", registry = \"suxen\" }\n",
		"src/lib.rs":         "pub use hello::VERSION;\n",
		".cargo/config.toml": "[registries.suxen]\nindex = \"sparse+" + f.suxen.URL + "/repository/" + repository + "/\"\ncredential-provider = \"cargo:token\"\n",
	}
	for path, content := range files {
		full := filepath.Join(directory, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// cargoEnvironment gives cargo a private CARGO_HOME (its config, credentials,
// and registry cache) while leaving HOME alone so a rustup-managed toolchain
// is still found.
func cargoEnvironment(t *testing.T) []string {
	t.Helper()
	return append(os.Environ(),
		"CARGO_HOME="+filepath.Join(t.TempDir(), "cargo"),
		"CARGO_REGISTRIES_SUXEN_TOKEN="+adminToken,
		"CARGO_TERM_COLOR=never",
		"CARGO_NET_RETRY=0",
	)
}

func runCargo(t *testing.T, environment []string, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("cargo", args...)
	command.Dir = directory
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("cargo %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// cargoPublishBody frames a metadata JSON and a .crate the way `cargo publish`
// does: each is a little-endian uint32 length followed by its bytes.
func cargoPublishBody(t *testing.T, metadata string, crate []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(metadata)))
	buffer.Write(length[:])
	buffer.WriteString(metadata)
	binary.LittleEndian.PutUint32(length[:], uint32(len(crate)))
	buffer.Write(length[:])
	buffer.Write(crate)
	return buffer.Bytes()
}

func TestCargoHostedBareTokenPublishOverHTTP(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))

	crate := crateFile(t, "widget", "1.0.0")
	body := cargoPublishBody(t, `{"name":"widget","vers":"1.0.0","deps":[],"features":{}}`, crate)

	// Cargo sends its token with no scheme; the request must authenticate
	// through the bare-token path and be authorized to write.
	request, err := http.NewRequest(http.MethodPut, f.suxen.URL+"/repository/hosted/api/v1/crates/new", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", adminToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("bare-token publish: %d %s", response.StatusCode, content)
	}

	// A bare token that does not match a stored token stays anonymous, so the
	// write is refused.
	request, _ = http.NewRequest(http.MethodPut, f.suxen.URL+"/repository/hosted/api/v1/crates/new", bytes.NewReader(body))
	request.Header.Set("Authorization", "not-a-real-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		t.Fatalf("publish with a bad token was not refused: %d", response.StatusCode)
	}

	// The synthesized index advertises the version with the stored crate's own
	// checksum, and config.json points dl/api at the request host.
	response2, index := f.do(t, http.MethodGet, "/repository/hosted/wi/dg/widget", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
	})
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("index: %d %s", response2.StatusCode, index)
	}
	sum := sha256.Sum256(crate)
	if !strings.Contains(string(index), `"vers":"1.0.0"`) || !strings.Contains(string(index), `"cksum":"`+hex.EncodeToString(sum[:])+`"`) {
		t.Fatalf("index entry = %s", index)
	}
	response2, config := f.do(t, http.MethodGet, "/repository/hosted/config.json", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
	})
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("config: %d %s", response2.StatusCode, config)
	}
	var configDocument struct {
		Download     string `json:"dl"`
		API          string `json:"api"`
		AuthRequired bool   `json:"auth-required"`
	}
	if err := json.Unmarshal(config, &configDocument); err != nil {
		t.Fatal(err)
	}
	if configDocument.API != "https://mirror.example/repository/hosted" ||
		configDocument.Download != "https://mirror.example/repository/hosted/dl/{crate}/{version}/download" ||
		!configDocument.AuthRequired {
		t.Fatalf("config = %s", config)
	}

	// The stored crate downloads byte-for-byte.
	response2, download := f.do(t, http.MethodGet, "/repository/hosted/dl/widget/1.0.0/download", nil, nil)
	if response2.StatusCode != http.StatusOK || !bytes.Equal(download, crate) {
		t.Fatalf("download: %d (%d bytes)", response2.StatusCode, len(download))
	}
}

func TestCargoPublishRejectsOversizedAndTruncatedFrames(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "hosted", "format": "cargo", "type": "hosted",
	}))
	var oversized [4]byte
	binary.LittleEndian.PutUint32(oversized[:], ^uint32(0))
	response, body := f.do(
		t, http.MethodPut, "/repository/hosted/api/v1/crates/new", oversized[:], nil,
	)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized metadata status = %d, want 413: %s", response.StatusCode, body)
	}

	metadata := `{"name":"truncated","vers":"1.0.0","deps":[],"features":{}}`
	var framed bytes.Buffer
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(metadata)))
	framed.Write(length[:])
	framed.WriteString(metadata)
	binary.LittleEndian.PutUint32(length[:], 100)
	framed.Write(length[:])
	framed.WriteString("short")
	response, body = f.do(
		t, http.MethodPut, "/repository/hosted/api/v1/crates/new", framed.Bytes(), nil,
	)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("truncated crate status = %d, want 400: %s", response.StatusCode, body)
	}
	response, body = f.do(
		t, http.MethodGet, "/repository/hosted/api/v1/crates/truncated/1.0.0/download", nil, nil,
	)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("truncated crate became visible: %d %s", response.StatusCode, body)
	}
}

func TestCargoHostedPublishAndFetch(t *testing.T) {
	requireCargo(t)
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))

	publishEnvironment := append(cargoEnvironment(t), "CARGO_REGISTRIES_SUXEN_TOKEN="+adminToken)
	pkg := t.TempDir()
	for path, content := range map[string]string{
		"Cargo.toml": "[package]\nname = \"hello\"\nversion = \"0.1.0\"\nedition = \"2021\"\n" +
			"description = \"hello crate\"\nlicense = \"MIT\"\n\n[lib]\npath = \"src/lib.rs\"\n",
		"src/lib.rs":         "pub const VERSION: &str = \"0.1.0\";\n",
		".cargo/config.toml": "[registries.suxen]\nindex = \"sparse+" + f.suxen.URL + "/repository/hosted/\"\ncredential-provider = \"cargo:token\"\n",
	} {
		full := filepath.Join(pkg, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCargo(t, publishEnvironment, pkg, "publish", "--registry", "suxen", "--no-verify", "--allow-dirty")

	// The synthesized index shows the published version.
	response, index := f.do(t, http.MethodGet, "/repository/hosted/he/ll/hello", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(index), `"vers":"0.1.0"`) {
		t.Fatalf("index after publish: %d %s", response.StatusCode, index)
	}

	// A consumer resolves and downloads the published crate through suxen.
	project := newProject(t, f, "hosted")
	runCargo(t, cargoEnvironment(t), project, "fetch")
}

func TestCargoProxyFetchesThroughSuxenAndReplaysFromCache(t *testing.T) {
	requireCargo(t)
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"hello": {"0.1.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "crates", "format": "cargo", "type": "proxy", "upstream": reg.index.URL}))

	project := newProject(t, f, "crates")
	runCargo(t, cargoEnvironment(t), project, "fetch")
	lock, err := os.ReadFile(filepath.Join(project, "Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), "sparse+"+f.suxen.URL+"/repository/crates/") {
		t.Fatalf("lockfile does not reference the suxen registry:\n%s", lock)
	}
	// config.json, the index file, and the crate: nothing more went upstream.
	if hits := reg.hits.Load(); hits != 3 {
		t.Fatalf("upstream hits = %d, want 3", hits)
	}

	reg.blocked.Store(true)
	runCargo(t, cargoEnvironment(t), project, "fetch")

	response, body := f.do(t, http.MethodGet, "/api/v1/repositories/crates/assets", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list assets: %d %s", response.StatusCode, body)
	}
	for _, want := range []string{`"dl/hello/0.1.0/download"`, `"name":"hello"`, `"version":"0.1.0"`, `"he/ll/hello"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("asset listing lacks %s:\n%s", want, body)
		}
	}
}

func TestCargoProxyRetainedCrateSurvivesConfigDeletion(t *testing.T) {
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"hello": {"0.1.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "crates", "format": "cargo", "type": "proxy", "upstream": reg.index.URL}))

	cratePath := "/repository/crates/dl/hello/0.1.0/download"
	response, body := f.do(t, http.MethodGet, "/repository/crates/config.json", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("config fetch: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/crates/he/ll/hello", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("index fetch: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, cratePath, nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, reg.crates["hello"]["0.1.0"]) {
		t.Fatalf("initial crate fetch: %d (%d bytes)", response.StatusCode, len(body))
	}

	response, body = f.do(t, http.MethodGet, "/api/v1/repositories/crates/assets?prefix=config.json", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list config: %d %s", response.StatusCode, body)
	}
	var page struct {
		Items []domain.Asset `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "config.json" {
		t.Fatalf("config assets: %+v", page.Items)
	}
	response, body = f.do(t, http.MethodDelete, "/api/v1/repositories/crates/assets/"+strconv.FormatInt(page.Items[0].ID, 10), nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete config: %d %s", response.StatusCode, body)
	}
	reg.blocked.Store(true)
	upstreamHits := reg.hits.Load()
	response, body = f.do(t, http.MethodGet, cratePath, nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, reg.crates["hello"]["0.1.0"]) {
		t.Fatalf("retained crate fetch: %d (%d bytes)", response.StatusCode, len(body))
	}
	if hits := reg.hits.Load(); hits != upstreamHits {
		t.Fatalf("retained crate made %d upstream requests", hits-upstreamHits)
	}
}

func TestCargoConfigIsRewrittenPerRequestAndStoredVerbatim(t *testing.T) {
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"hello": {"0.1.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "crates", "format": "cargo", "type": "proxy", "upstream": reg.index.URL}))

	response, body := f.do(t, http.MethodGet, "/repository/crates/config.json", nil, http.Header{
		"Host":              {"mirror.example:8443"},
		"X-Forwarded-Proto": {"https"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("config: %d %s", response.StatusCode, body)
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	if config["dl"] != "https://mirror.example:8443/repository/crates/dl/{crate}/{version}/download" {
		t.Fatalf("dl = %v", config["dl"])
	}
	if _, present := config["api"]; present {
		t.Fatal("api leaked through")
	}

	response, body = f.do(t, http.MethodGet, "/repository/crates/config.json", nil, http.Header{"Host": {"other.internal"}})
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "http://other.internal/repository/crates/dl/") {
		t.Fatalf("second host config = %d %s", response.StatusCode, body)
	}

	// The stored asset is the upstream's bytes: its digest is the upstream
	// config's sha256, not the rewritten body's.
	upstreamConfig := []byte(`{"dl":"` + reg.download.URL + `/crates","api":"https://crates.example"}`)
	sum := sha256.Sum256(upstreamConfig)
	response, body = f.do(t, http.MethodGet, "/api/v1/repositories/crates/assets", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"sha256:`+hex.EncodeToString(sum[:])+`"`) {
		t.Fatalf("stored config digest is not the upstream digest:\n%s", body)
	}
}

func TestCargoGroupMergesIndexAndPointsConfigAtGroup(t *testing.T) {
	f := newFixture(t, time.Hour)
	first := newRegistry(t, map[string][]string{"hello": {"0.1.0"}})
	second := newRegistry(t, map[string][]string{"hello": {"0.2.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "cargo", "type": "proxy", "upstream": first.index.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "cargo", "type": "proxy", "upstream": second.index.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "all", "format": "cargo", "type": "group", "members": []string{"first", "second"}}))

	response, body := f.do(t, http.MethodGet, "/repository/all/he/ll/hello", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group index: %d %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"vers":"0.1.0"`) || !strings.Contains(string(body), `"vers":"0.2.0"`) {
		t.Fatalf("group index lacks a member version:\n%s", body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/all/config.json", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "/repository/all/dl/{crate}/{version}/download") {
		t.Fatalf("group config = %d %s", response.StatusCode, body)
	}
	// The group's download path resolves to the member that has the version,
	// which in turn resolves the download host from its own cached config.
	response, body = f.do(t, http.MethodGet, "/repository/all/dl/hello/0.2.0/download", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, second.crates["hello"]["0.2.0"]) {
		t.Fatalf("group crate download: %d (%d bytes)", response.StatusCode, len(body))
	}

	hosted := f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"})
	if hosted.StatusCode != http.StatusCreated {
		t.Fatalf("hosted cargo repository rejected: %d", hosted.StatusCode)
	}
}
