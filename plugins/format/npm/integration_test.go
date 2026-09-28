package npm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/server"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/npm"
)

const adminToken = "npm-integration-admin-token"

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

func (f *fixture) createRepository(t *testing.T, spec map[string]any) int {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	response, content := f.do(t, http.MethodPost, "/api/v1/repositories", body, http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("create repository %v: %d %s", spec["name"], response.StatusCode, content)
	}
	return response.StatusCode
}

func mustCreate(t *testing.T, status int) {
	t.Helper()
	if status != http.StatusCreated {
		t.Fatalf("create repository: status %d", status)
	}
}

// tarball builds a minimal npm package tarball (package/… layout).
func tarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	manifest, _ := json.Marshal(map[string]any{"name": name, "version": version, "main": "index.js"})
	for path, content := range map[string]string{
		"package/package.json": string(manifest),
		"package/index.js":     "module.exports = " + strconv(version) + ";\n",
	} {
		header := &tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(0, 0)}
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

func strconv(value string) string {
	return `"` + value + `"`
}

// registry is a fake npm registry whose tarballs live on a second host, as
// some registries do, so the resolver path is exercised.
type registry struct {
	metadata *httptest.Server
	files    *httptest.Server
	hits     atomic.Int64
	blocked  atomic.Bool
	packages map[string]map[string][]byte // name -> version -> tarball
}

func newRegistry(t *testing.T, packages map[string][]string) *registry {
	t.Helper()
	r := &registry{packages: map[string]map[string][]byte{}}
	for name, versions := range packages {
		r.packages[name] = map[string][]byte{}
		for _, version := range versions {
			r.packages[name][version] = tarball(t, name, version)
		}
	}
	r.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if r.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		// /<name>/-/<basename>-<version>.tgz
		name, file, found := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/-/")
		if !found {
			http.NotFound(w, req)
			return
		}
		basename := name[strings.LastIndex(name, "/")+1:]
		version := strings.TrimSuffix(strings.TrimPrefix(file, basename+"-"), ".tgz")
		content, ok := r.packages[name][version]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	}))
	t.Cleanup(r.files.Close)
	r.metadata = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if r.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(req.URL.Path, "/")
		versions, ok := r.packages[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		document := map[string]any{"name": name, "versions": map[string]any{}, "dist-tags": map[string]any{}}
		latest := ""
		basename := name[strings.LastIndex(name, "/")+1:]
		for version, content := range versions {
			sum512 := sha512.Sum512(content)
			sum1 := sha1.Sum(content)
			document["versions"].(map[string]any)[version] = map[string]any{
				"name": name, "version": version,
				"dist": map[string]any{
					"tarball":   r.files.URL + "/" + name + "/-/" + basename + "-" + version + ".tgz",
					"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum512[:]),
					"shasum":    hex.EncodeToString(sum1[:]),
				},
			}
			if latest == "" || version > latest {
				latest = version
			}
		}
		document["dist-tags"].(map[string]any)["latest"] = latest
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(document)
	}))
	t.Cleanup(r.metadata.Close)
	return r
}

func requireNpm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm binary not available")
	}
}

func npmEnvironment(t *testing.T, f *fixture, repository string) []string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"npm_config_userconfig="+filepath.Join(home, ".npmrc"),
		"npm_config_cache="+filepath.Join(home, "cache"),
		"npm_config_registry="+f.suxen.URL+"/repository/"+repository+"/",
		"npm_config_audit=false",
		"npm_config_fund=false",
		"npm_config_update_notifier=false",
		"npm_config_loglevel=error",
		"NO_COLOR=1",
	)
}

func newProject(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(`{"name":"consumer","version":"1.0.0","private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func runNpm(t *testing.T, environment []string, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("npm", args...)
	command.Dir = directory
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func installedVersion(t *testing.T, project, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(project, "node_modules", name, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Version
}

// hostedNpmEnvironment configures npm to publish to and install from a hosted
// suxen repository, carrying the admin token as the registry auth token.
func hostedNpmEnvironment(t *testing.T, f *fixture, repository string) []string {
	t.Helper()
	home := t.TempDir()
	registry := f.suxen.URL + "/repository/" + repository + "/"
	hostPort := strings.TrimPrefix(f.suxen.URL, "http://")
	npmrc := "registry=" + registry + "\n" +
		"//" + hostPort + "/repository/" + repository + "/:_authToken=" + adminToken + "\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(npmrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"npm_config_userconfig="+filepath.Join(home, ".npmrc"),
		"npm_config_cache="+filepath.Join(home, "cache"),
		"npm_config_audit=false",
		"npm_config_fund=false",
		"npm_config_update_notifier=false",
		"npm_config_loglevel=error",
		"NO_COLOR=1",
	)
}

// newPackage writes a minimal publishable package directory.
func newPackage(t *testing.T, name, version string) string {
	t.Helper()
	directory := t.TempDir()
	manifest := fmt.Sprintf(`{"name":%q,"version":%q}`, name, version)
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.js"), []byte("module.exports = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestNpmHostedPublishAndInstall(t *testing.T) {
	requireNpm(t)
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))

	// The packument 404s until something is published.
	if response, _ := f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("packument for unpublished package: %d", response.StatusCode)
	}

	runNpm(t, hostedNpmEnvironment(t, f, "hosted"), newPackage(t, "widget", "1.0.0"), "publish")
	runNpm(t, hostedNpmEnvironment(t, f, "hosted"), newPackage(t, "widget", "1.1.0"), "publish")

	// An install from a fresh cache resolves through suxen; a successful
	// install also proves the stored tarball matches the published integrity.
	project := newProject(t)
	runNpm(t, hostedNpmEnvironment(t, f, "hosted"), project, "install", "widget@1.0.0")
	if got := installedVersion(t, project, "widget"); got != "1.0.0" {
		t.Fatalf("installed %s", got)
	}
	lock, err := os.ReadFile(filepath.Join(project, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), f.suxen.URL+"/repository/hosted/widget/-/widget-1.0.0.tgz") {
		t.Fatalf("lockfile does not resolve through suxen:\n%s", lock)
	}

	// latest is the higher release and integrity survived the round trip.
	response, body := f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("packument: %d %s", response.StatusCode, body)
	}
	var document struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Versions) != 2 || document.DistTags["latest"] != "1.1.0" {
		t.Fatalf("packument = %s", body)
	}
	if !strings.HasPrefix(document.Versions["1.0.0"].Dist.Integrity, "sha512-") {
		t.Fatal("integrity lost")
	}
	if got := document.Versions["1.0.0"].Dist.Tarball; got != f.suxen.URL+"/repository/hosted/widget/-/widget-1.0.0.tgz" {
		t.Fatalf("tarball URL = %q", got)
	}

	latestProject := newProject(t)
	runNpm(t, hostedNpmEnvironment(t, f, "hosted"), latestProject, "install", "widget")
	if got := installedVersion(t, latestProject, "widget"); got != "1.1.0" {
		t.Fatalf("latest install got %s", got)
	}
}

// TestNpmHostedScopedPublishViaHTTP drives the publish protocol directly so a
// scoped package and the request-host rewrite are exercised without npm's
// scoped-registry auth quirks.
func TestNpmHostedScopedPublishViaHTTP(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))

	tar := tarball(t, "@acme/widget", "2.0.0")
	sum512 := sha512.Sum512(tar)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum512[:])
	publishBody, err := json.Marshal(map[string]any{
		"_id":  "@acme/widget",
		"name": "@acme/widget",
		"versions": map[string]any{
			"2.0.0": map[string]any{
				"name":    "@acme/widget",
				"version": "2.0.0",
				"dist": map[string]any{
					"tarball":   "http://publish-client/@acme/widget/-/widget-2.0.0.tgz",
					"integrity": integrity,
				},
			},
		},
		"_attachments": map[string]any{
			"widget-2.0.0.tgz": map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tar),
				"length":       len(tar),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, content := f.do(t, http.MethodPut, "/repository/hosted/@acme%2fwidget", publishBody, http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish: %d %s", response.StatusCode, content)
	}

	// The packument carries the request host and keeps the integrity.
	response, body := f.do(t, http.MethodGet, "/repository/hosted/@acme%2fwidget", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("packument: %d %s", response.StatusCode, body)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	dist := document.Versions["2.0.0"].Dist
	if dist.Tarball != "https://mirror.example/repository/hosted/@acme/widget/-/widget-2.0.0.tgz" {
		t.Fatalf("tarball = %q", dist.Tarball)
	}
	if dist.Integrity != integrity {
		t.Fatalf("integrity = %q, want %q", dist.Integrity, integrity)
	}

	// The stored tarball is byte-identical to what was published.
	response, body = f.do(t, http.MethodGet, "/repository/hosted/@acme/widget/-/widget-2.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, tar) {
		t.Fatalf("tarball: %d (%d bytes)", response.StatusCode, len(body))
	}
}

func TestNpmProxyInstallsThroughSuxenAndReplaysFromCache(t *testing.T) {
	requireNpm(t)
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"hello": {"1.0.0", "1.1.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "npm", "format": "npm", "type": "proxy", "upstream": reg.metadata.URL}))

	project := newProject(t)
	runNpm(t, npmEnvironment(t, f, "npm"), project, "install", "hello@1.0.0")
	if got := installedVersion(t, project, "hello"); got != "1.0.0" {
		t.Fatalf("installed %s", got)
	}
	lock, err := os.ReadFile(filepath.Join(project, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), f.suxen.URL+"/repository/npm/hello/-/hello-1.0.0.tgz") {
		t.Fatalf("lockfile does not resolve through suxen:\n%s", lock)
	}
	// One packument and one tarball went upstream.
	if hits := reg.hits.Load(); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
	versions := runNpm(t, npmEnvironment(t, f, "npm"), project, "view", "hello", "versions", "--json")
	if !strings.Contains(versions, `"1.0.0"`) || !strings.Contains(versions, `"1.1.0"`) {
		t.Fatalf("versions = %s", versions)
	}

	reg.blocked.Store(true)
	replay := newProject(t)
	runNpm(t, npmEnvironment(t, f, "npm"), replay, "install", "hello@1.0.0")
	if got := installedVersion(t, replay, "hello"); got != "1.0.0" {
		t.Fatalf("replayed install got %s", got)
	}

	response, body := f.do(t, http.MethodGet, "/api/v1/repositories/npm/assets", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list assets: %d %s", response.StatusCode, body)
	}
	for _, want := range []string{`"hello/-/hello-1.0.0.tgz"`, `"name":"hello"`, `"version":"1.0.0"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("asset listing lacks %s:\n%s", want, body)
		}
	}
}

func TestNpmScopedPackumentRewriteAndTarballResolution(t *testing.T) {
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"@acme/hello": {"2.0.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "npm", "format": "npm", "type": "proxy", "upstream": reg.metadata.URL}))

	response, body := f.do(t, http.MethodGet, "/repository/npm/@acme%2fhello", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("packument: %d %s", response.StatusCode, body)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	dist := document.Versions["2.0.0"].Dist
	if dist.Tarball != "https://mirror.example/repository/npm/@acme/hello/-/hello-2.0.0.tgz" {
		t.Fatalf("tarball = %q", dist.Tarball)
	}
	if !strings.HasPrefix(dist.Integrity, "sha512-") {
		t.Fatal("integrity lost")
	}

	// The tarball lives on the files host; the resolver finds it through the
	// cached packument, and the served bytes are the upstream's.
	response, body = f.do(t, http.MethodGet, "/repository/npm/@acme/hello/-/hello-2.0.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, reg.packages["@acme/hello"]["2.0.0"]) {
		t.Fatalf("tarball: %d (%d bytes)", response.StatusCode, len(body))
	}
	if hits := reg.hits.Load(); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
}

func TestNpmGroupMergesPackuments(t *testing.T) {
	f := newFixture(t, time.Hour)
	first := newRegistry(t, map[string][]string{"hello": {"1.0.0"}})
	second := newRegistry(t, map[string][]string{"hello": {"1.2.0"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "npm", "type": "proxy", "upstream": first.metadata.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "npm", "type": "proxy", "upstream": second.metadata.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "all", "format": "npm", "type": "group", "members": []string{"first", "second"}}))

	response, body := f.do(t, http.MethodGet, "/repository/all/hello", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group packument: %d %s", response.StatusCode, body)
	}
	var document struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Versions) != 2 || document.DistTags["latest"] != "1.2.0" {
		t.Fatalf("merged packument = %s", body)
	}
	if got := document.Versions["1.2.0"].Dist.Tarball; got != f.suxen.URL+"/repository/all/hello/-/hello-1.2.0.tgz" {
		t.Fatalf("group tarball URL = %q", got)
	}
	response, body = f.do(t, http.MethodGet, "/repository/all/hello/-/hello-1.2.0.tgz", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, second.packages["hello"]["1.2.0"]) {
		t.Fatalf("group tarball: %d (%d bytes)", response.StatusCode, len(body))
	}

	if status := f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}); status != http.StatusCreated {
		t.Fatalf("hosted npm repository rejected: %d", status)
	}
}
