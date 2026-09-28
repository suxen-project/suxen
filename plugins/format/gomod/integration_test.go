package gomod_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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
	_ "github.com/suxen-project/suxen/plugins/format/gomod"
)

const (
	adminToken    = "go-integration-admin-token"
	adminUser     = "admin"
	adminPassword = "integration-password"
	modulePath    = "example.com/hello"
)

type fixture struct {
	handler *server.Server
	suxen   *httptest.Server
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
		BootstrapUser:     adminUser,
		BootstrapPassword: adminPassword,
		BootstrapToken:    adminToken,
		MaxUploadBytes:    16 << 20,
		ProxyManifestTTL:  proxyTTL,
		OutboundTimeout:   5 * time.Second,
		// The upstream is a loopback httptest server, which the egress guard
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
	return &fixture{handler: handler, suxen: suxen}
}

func (f *fixture) do(t *testing.T, method, requestPath string, body []byte, contentType string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, f.suxen.URL+requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
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

func (f *fixture) createRepository(t *testing.T, spec map[string]any) {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	response, content := f.do(t, http.MethodPost, "/api/v1/repositories", body, "application/json")
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create repository %v: %d %s", spec["name"], response.StatusCode, content)
	}
}

func (f *fixture) upload(t *testing.T, repository, assetPath string, content []byte) (*http.Response, []byte) {
	t.Helper()
	return f.do(t, http.MethodPut, "/repository/"+repository+"/"+assetPath, content, "application/octet-stream")
}

func (f *fixture) get(t *testing.T, repository, assetPath string) (*http.Response, []byte) {
	t.Helper()
	return f.do(t, http.MethodGet, "/repository/"+repository+"/"+assetPath, nil, "")
}

func mustStatus(t *testing.T, response *http.Response, body []byte, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("%s: status = %d, want %d: %s", response.Request.URL.Path, response.StatusCode, want, body)
	}
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

// moduleFiles builds the .info, .mod, and .zip a GOPROXY serves for one
// version of the test module. The zip follows the module zip layout, all
// files under <module>@<version>/.
func moduleFiles(t *testing.T, version string) (info, mod, zipContent []byte) {
	t.Helper()
	info = []byte(`{"Version":"` + version + `","Time":"2024-01-01T00:00:00Z"}`)
	mod = []byte("module " + modulePath + "\n\ngo 1.21\n")
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	prefix := modulePath + "@" + version + "/"
	for name, content := range map[string]string{
		"go.mod":   string(mod),
		"hello.go": "package hello\n\n// Version is the module version.\nconst Version = \"" + version + "\"\n",
	} {
		entry, err := writer.Create(prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return info, mod, buffer.Bytes()
}

// upstream is a fake GOPROXY serving two versions of the test module.
type upstream struct {
	server  *httptest.Server
	hits    atomic.Int64
	blocked atomic.Bool
	files   map[string][]byte
}

func newUpstream(t *testing.T, versions ...string) *upstream {
	t.Helper()
	u := &upstream{files: map[string][]byte{}}
	for _, version := range versions {
		info, mod, zipContent := moduleFiles(t, version)
		u.files["/"+modulePath+"/@v/"+version+".info"] = info
		u.files["/"+modulePath+"/@v/"+version+".mod"] = mod
		u.files["/"+modulePath+"/@v/"+version+".zip"] = zipContent
	}
	u.files["/"+modulePath+"/@v/list"] = []byte(strings.Join(versions, "\n") + "\n")
	u.files["/"+modulePath+"/@latest"] = u.files["/"+modulePath+"/@v/"+versions[len(versions)-1]+".info"]
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if u.blocked.Load() {
			http.Error(w, "upstream blocked", http.StatusServiceUnavailable)
			return
		}
		content, found := u.files[r.URL.Path]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(u.server.Close)
	return u
}

// goEnvironment isolates the go command: its own module cache, no sumdb
// (the fake upstream cannot sign a checksum log), credentials via netrc.
func goEnvironment(t *testing.T, f *fixture, repository string) []string {
	t.Helper()
	home := t.TempDir()
	parsed, err := url.Parse(f.suxen.URL)
	if err != nil {
		t.Fatal(err)
	}
	netrc := "machine " + parsed.Hostname() + " login " + adminUser + " password " + adminPassword + "\n"
	if err := os.WriteFile(filepath.Join(home, "netrc"), []byte(netrc), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(),
		"HOME="+home,
		"NETRC="+filepath.Join(home, "netrc"),
		"GOPROXY="+f.suxen.URL+"/repository/"+repository,
		"GOSUMDB=off",
		"GONOSUMDB=",
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"GOMODCACHE="+filepath.Join(home, "modcache"),
		"GOPATH="+filepath.Join(home, "gopath"),
		"GOCACHE="+filepath.Join(home, "gocache"),
		"GOENV=off",
	)
	// The module cache is written read-only; let go remove it so TempDir
	// cleanup does not fail on permissions.
	t.Cleanup(func() { _, _ = tryGo(environment, home, "clean", "-modcache") })
	return environment
}

func newConsumerModule(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module consumer\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func runGo(t *testing.T, environment []string, directory string, args ...string) string {
	t.Helper()
	output, err := tryGo(environment, directory, args...)
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func tryGo(environment []string, directory string, args ...string) (string, error) {
	command := exec.Command("go", args...)
	command.Dir = directory
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestGoProxyDownloadsAndServesFromCache(t *testing.T) {
	f := newFixture(t, time.Hour)
	up := newUpstream(t, "v1.0.0", "v1.1.0")
	f.createRepository(t, map[string]any{"name": "goproxy", "format": "go", "type": "proxy", "upstream": up.server.URL})

	consumer := newConsumerModule(t)
	output := runGo(t, goEnvironment(t, f, "goproxy"), consumer, "mod", "download", "-json", modulePath+"@v1.0.0")
	if !strings.Contains(output, `"Version": "v1.0.0"`) {
		t.Fatalf("download output: %s", output)
	}
	versions := runGo(t, goEnvironment(t, f, "goproxy"), consumer, "list", "-m", "-versions", modulePath)
	if strings.TrimSpace(versions) != modulePath+" v1.0.0 v1.1.0" {
		t.Fatalf("versions = %q", versions)
	}
	// "latest" resolves through @v/list to the highest release.
	output = runGo(t, goEnvironment(t, f, "goproxy"), consumer, "mod", "download", "-json", modulePath+"@latest")
	if !strings.Contains(output, `"Version": "v1.1.0"`) {
		t.Fatalf("latest output: %s", output)
	}

	up.blocked.Store(true)
	// A fresh module cache and a blocked upstream: everything comes from suxen.
	replay := runGo(t, goEnvironment(t, f, "goproxy"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@v1.0.0")
	if !strings.Contains(replay, `"Version": "v1.0.0"`) {
		t.Fatalf("replay output: %s", replay)
	}
	if output, err := tryGo(goEnvironment(t, f, "goproxy"), consumer, "list", "-m", "-versions", modulePath); err != nil ||
		strings.TrimSpace(output) != modulePath+" v1.0.0 v1.1.0" {
		t.Fatalf("cached list = %q %v", output, err)
	}

	response, body := f.do(t, http.MethodGet, "/api/v1/repositories/goproxy/assets", nil, "")
	mustStatus(t, response, body, http.StatusOK)
	for _, want := range []string{`"example.com/hello/@v/v1.0.0.zip"`, `"module":"example.com/hello"`, `"version":"v1.0.0"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("asset listing lacks %s:\n%s", want, body)
		}
	}
}

func TestGoProxyRevalidatesListOnTTL(t *testing.T) {
	const ttl = 2 * time.Second
	f := newFixture(t, ttl)
	up := newUpstream(t, "v1.0.0")
	f.createRepository(t, map[string]any{"name": "goproxy", "format": "go", "type": "proxy", "upstream": up.server.URL})

	response, body := f.get(t, "goproxy", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\n" {
		t.Fatalf("list = %q", body)
	}
	hits := up.hits.Load()

	info, mod, zipContent := moduleFiles(t, "v1.2.0")
	up.files["/"+modulePath+"/@v/v1.2.0.info"] = info
	up.files["/"+modulePath+"/@v/v1.2.0.mod"] = mod
	up.files["/"+modulePath+"/@v/v1.2.0.zip"] = zipContent
	up.files["/"+modulePath+"/@v/list"] = []byte("v1.0.0\nv1.2.0\n")

	response, body = f.get(t, "goproxy", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\n" || up.hits.Load() != hits {
		t.Fatalf("list within TTL = %q (hits %d → %d)", body, hits, up.hits.Load())
	}

	time.Sleep(ttl + 500*time.Millisecond)
	response, body = f.get(t, "goproxy", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\nv1.2.0\n" {
		t.Fatalf("list after TTL = %q", body)
	}
	// The .zip is immutable: re-reading it never revalidates.
	response, body = f.get(t, "goproxy", modulePath+"/@v/v1.0.0.zip")
	mustStatus(t, response, body, http.StatusOK)
	hits = up.hits.Load()
	response, body = f.get(t, "goproxy", modulePath+"/@v/v1.0.0.zip")
	mustStatus(t, response, body, http.StatusOK)
	if up.hits.Load() != hits {
		t.Fatal("immutable zip was revalidated")
	}
}

func TestGoHostedSynthesizesListAndLatest(t *testing.T) {
	f := newFixture(t, time.Hour)
	f.createRepository(t, map[string]any{"name": "gohosted", "format": "go", "type": "hosted"})

	for _, version := range []string{"v1.0.0", "v1.0.1"} {
		info, mod, zipContent := moduleFiles(t, version)
		for extension, content := range map[string][]byte{".info": info, ".mod": mod, ".zip": zipContent} {
			response, body := f.upload(t, "gohosted", modulePath+"/@v/"+version+extension, content)
			mustStatus(t, response, body, http.StatusCreated)
		}
	}

	response, body := f.get(t, "gohosted", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\nv1.0.1\n" {
		t.Fatalf("list = %q", body)
	}
	response, body = f.get(t, "gohosted", modulePath+"/@latest")
	mustStatus(t, response, body, http.StatusOK)
	if !strings.Contains(string(body), `"v1.0.1"`) {
		t.Fatalf("latest = %q", body)
	}

	rejected := map[string]string{
		modulePath + "/@v/list":        "go_derived_path",
		modulePath + "/@v/master.info": "go_invalid_version",
		modulePath + "/hello.tgz":      "go_invalid_path",
	}
	for assetPath, wantCode := range rejected {
		response, body := f.upload(t, "gohosted", assetPath, []byte("x"))
		mustStatus(t, response, body, http.StatusBadRequest)
		if code := problemCode(t, body); code != wantCode {
			t.Fatalf("%s: code = %s, want %s", assetPath, code, wantCode)
		}
	}

	output := runGo(t, goEnvironment(t, f, "gohosted"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@latest")
	if !strings.Contains(output, `"Version": "v1.0.1"`) {
		t.Fatalf("hosted download: %s", output)
	}
}

func TestGoHostedDoesNotAdvertiseIncompleteVersion(t *testing.T) {
	f := newFixture(t, time.Hour)
	f.createRepository(t, map[string]any{"name": "gohosted", "format": "go", "type": "hosted"})
	for _, version := range []string{"v1.0.0", "v1.1.0"} {
		info, mod, zipContent := moduleFiles(t, version)
		files := map[string][]byte{".info": info, ".mod": mod, ".zip": zipContent}
		if version == "v1.1.0" {
			files = map[string][]byte{".mod": mod}
		}
		for extension, content := range files {
			response, body := f.upload(t, "gohosted", modulePath+"/@v/"+version+extension, content)
			mustStatus(t, response, body, http.StatusCreated)
		}
	}

	response, body := f.get(t, "gohosted", modulePath+"/@v/v1.1.0.mod")
	mustStatus(t, response, body, http.StatusOK)
	if !bytes.Contains(body, []byte("module "+modulePath)) {
		t.Fatalf("uploaded partial .mod not readable: %q", body)
	}
	response, body = f.get(t, "gohosted", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\n" {
		t.Fatalf("incomplete version appeared in list: %q", body)
	}
	response, body = f.get(t, "gohosted", modulePath+"/@latest")
	mustStatus(t, response, body, http.StatusOK)
	if !strings.Contains(string(body), `"Version":"v1.0.0"`) {
		t.Fatalf("incomplete version displaced latest: %q", body)
	}
	output := runGo(t, goEnvironment(t, f, "gohosted"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@latest")
	if !strings.Contains(output, `"Version": "v1.0.0"`) {
		t.Fatalf("go downloaded unexpected latest: %s", output)
	}

	info, _, zipContent := moduleFiles(t, "v1.1.0")
	for extension, content := range map[string][]byte{".info": info, ".zip": zipContent} {
		response, body := f.upload(t, "gohosted", modulePath+"/@v/v1.1.0"+extension, content)
		mustStatus(t, response, body, http.StatusCreated)
	}
	response, body = f.get(t, "gohosted", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\nv1.1.0\n" {
		t.Fatalf("completed version absent from list: %q", body)
	}
	response, body = f.get(t, "gohosted", modulePath+"/@latest")
	mustStatus(t, response, body, http.StatusOK)
	if !strings.Contains(string(body), `"Version":"v1.1.0"`) {
		t.Fatalf("completed version absent from latest: %q", body)
	}
}

func TestGoGroupMergesListsAndLatest(t *testing.T) {
	f := newFixture(t, time.Hour)
	up := newUpstream(t, "v1.0.0", "v1.1.0")
	f.createRepository(t, map[string]any{"name": "goproxy", "format": "go", "type": "proxy", "upstream": up.server.URL})
	f.createRepository(t, map[string]any{"name": "gohosted", "format": "go", "type": "hosted"})
	info, mod, zipContent := moduleFiles(t, "v1.5.0")
	for extension, content := range map[string][]byte{".info": info, ".mod": mod, ".zip": zipContent} {
		response, body := f.upload(t, "gohosted", modulePath+"/@v/v1.5.0"+extension, content)
		mustStatus(t, response, body, http.StatusCreated)
	}
	f.createRepository(t, map[string]any{
		"name": "gogroup", "format": "go", "type": "group", "members": []string{"goproxy", "gohosted"},
	})

	response, body := f.get(t, "gogroup", modulePath+"/@v/list")
	mustStatus(t, response, body, http.StatusOK)
	if string(body) != "v1.0.0\nv1.1.0\nv1.5.0\n" {
		t.Fatalf("group list = %q", body)
	}
	response, body = f.get(t, "gogroup", modulePath+"/@latest")
	mustStatus(t, response, body, http.StatusOK)
	if !strings.Contains(string(body), `"v1.5.0"`) {
		t.Fatalf("group latest = %q", body)
	}

	output := runGo(t, goEnvironment(t, f, "gogroup"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@latest")
	if !strings.Contains(output, `"Version": "v1.5.0"`) {
		t.Fatalf("group download: %s", output)
	}
	output = runGo(t, goEnvironment(t, f, "gogroup"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@v1.1.0")
	if !strings.Contains(output, `"Version": "v1.1.0"`) {
		t.Fatalf("group proxy member download: %s", output)
	}
}

func TestGoLatestPseudoVersionUsesCommitTime(t *testing.T) {
	f := newFixture(t, time.Hour)
	older := "v1.2.1-0.20230101000000-abcdefabcdef"
	newer := "v0.0.0-20240101000000-123456789abc"
	for index, name := range []string{"older", "newer"} {
		f.createRepository(t, map[string]any{"name": name, "format": "go", "type": "hosted"})
		version := []string{older, newer}[index]
		info, mod, zipContent := moduleFiles(t, version)
		for extension, content := range map[string][]byte{".info": info, ".mod": mod, ".zip": zipContent} {
			response, body := f.upload(t, name, modulePath+"/@v/"+version+extension, content)
			mustStatus(t, response, body, http.StatusCreated)
		}
	}
	f.createRepository(t, map[string]any{
		"name": "gogroup", "format": "go", "type": "group", "members": []string{"older", "newer"},
	})
	// Neither member lists pseudo-versions in @v/list, so the Go client
	// must use the group's @latest answer to choose the revision to download.
	output := runGo(t, goEnvironment(t, f, "gogroup"), newConsumerModule(t), "mod", "download", "-json", modulePath+"@latest")
	if !strings.Contains(output, `"Version": "`+newer+`"`) {
		t.Fatalf("latest pseudo-version download: %s", output)
	}
}
