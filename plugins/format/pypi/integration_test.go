package pypi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
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
	_ "github.com/suxen-project/suxen/plugins/format/pypi"
)

const adminToken = "pypi-integration-admin-token"

type fixture struct {
	handler       *server.Server
	suxen         *httptest.Server
	dataDirectory string
}

func newFixture(t *testing.T, proxyTTL time.Duration, maxUploadBytes ...int64) *fixture {
	t.Helper()
	uploadLimit := int64(16 << 20)
	if len(maxUploadBytes) != 0 {
		uploadLimit = maxUploadBytes[0]
	}
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
		MaxUploadBytes:    uploadLimit,
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

// wheel builds a minimal valid pure-Python wheel.
func wheel(t *testing.T, name, version string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	distInfo := name + "-" + version + ".dist-info/"
	files := map[string]string{
		name + "/__init__.py": "VERSION = \"" + version + "\"\n",
		distInfo + "METADATA": "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n",
		distInfo + "WHEEL":    "Wheel-Version: 1.0\nGenerator: suxen-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		distInfo + "RECORD":   "",
	}
	for path, content := range files {
		entry, err := writer.Create(path)
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
	return buffer.Bytes()
}

type distribution struct {
	filename string
	content  []byte
	digest   string
	version  string
}

// index is a fake simple index whose files live on a second host, like
// pypi.org and files.pythonhosted.org.
type index struct {
	pages    *httptest.Server
	files    *httptest.Server
	hits     atomic.Int64
	blocked  atomic.Bool
	projects map[string][]distribution
	// html forces PEP 503 pages regardless of the client's Accept header.
	html bool
}

func newIndex(t *testing.T, projects map[string][]string, html bool) *index {
	t.Helper()
	idx := &index{projects: map[string][]distribution{}, html: html}
	for name, versions := range projects {
		for _, version := range versions {
			content := wheel(t, name, version)
			sum := sha256.Sum256(content)
			idx.projects[name] = append(idx.projects[name], distribution{
				filename: name + "-" + version + "-py3-none-any.whl",
				content:  content,
				digest:   hex.EncodeToString(sum[:]),
				version:  version,
			})
		}
	}
	idx.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx.hits.Add(1)
		if idx.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		// /packages/<digest>/<filename>[.metadata]
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/packages/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		for _, dists := range idx.projects {
			for _, dist := range dists {
				if dist.digest == parts[0] && dist.filename == parts[1] {
					_, _ = w.Write(dist.content)
					return
				}
				if dist.digest == parts[0] && dist.filename+".metadata" == parts[1] {
					_, _ = io.WriteString(
						w,
						"Metadata-Version: 2.1\nName: "+strings.SplitN(dist.filename, "-", 2)[0]+
							"\nVersion: "+dist.version+"\n",
					)
					return
				}
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idx.files.Close)
	idx.pages = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx.hits.Add(1)
		if idx.blocked.Load() {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		project := strings.Trim(strings.TrimPrefix(r.URL.Path, "/simple"), "/")
		if project == "" {
			w.Header().Set("Content-Type", "text/html")
			for name := range idx.projects {
				_, _ = io.WriteString(w, `<a href="`+idx.pages.URL+`/simple/`+name+`/">`+name+"</a>\n")
			}
			return
		}
		dists, found := idx.projects[project]
		if !found {
			http.NotFound(w, r)
			return
		}
		if idx.html || !strings.Contains(r.Header.Get("Accept"), "json") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>\n")
			for _, dist := range dists {
				_, _ = io.WriteString(w, `<a href="`+idx.files.URL+"/packages/"+dist.digest+"/"+dist.filename+"#sha256="+dist.digest+`" data-requires-python="&gt;=3.8" data-core-metadata="true">`+dist.filename+"</a>\n")
			}
			_, _ = io.WriteString(w, "</body></html>\n")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		files := make([]map[string]any, 0, len(dists))
		for _, dist := range dists {
			files = append(files, map[string]any{
				"filename":        dist.filename,
				"url":             idx.files.URL + "/packages/" + dist.digest + "/" + dist.filename,
				"hashes":          map[string]string{"sha256": dist.digest},
				"requires-python": ">=3.8",
				"core-metadata":   true,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"api-version": "1.0"}, "name": project, "files": files})
	}))
	t.Cleanup(idx.pages.Close)
	return idx
}

func requirePip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if output, err := exec.Command("python3", "-m", "pip", "--version").CombinedOutput(); err != nil {
		t.Skipf("pip not available: %s", output)
	}
}

func pipDownload(t *testing.T, f *fixture, repository, requirement string) string {
	t.Helper()
	dest := t.TempDir()
	command := exec.Command("python3", "-m", "pip", "download", requirement,
		"--index-url", f.suxen.URL+"/repository/"+repository+"/simple/",
		"--trusted-host", "127.0.0.1",
		"--dest", dest, "--no-deps", "--no-cache-dir", "--disable-pip-version-check", "--isolated", "--quiet",
	)
	command.Env = append(os.Environ(), "PIP_NO_INPUT=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pip download %s: %v\n%s", requirement, err, output)
	}
	return dest
}

func downloadedDigest(t *testing.T, dest, filename string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dest, filename))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// twineUpload posts a distribution the way `twine upload` does: a multipart
// form with the metadata fields and the file in the `content` part.
func twineUpload(t *testing.T, f *fixture, repository, project, version, filename string, content []byte) (*http.Response, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	sum := sha256.Sum256(content)
	fields := map[string]string{
		":action":          "file_upload",
		"protocol_version": "1",
		"name":             project,
		"version":          version,
		"filetype":         "bdist_wheel",
		"pyversion":        "py3",
		"metadata_version": "2.1",
		"sha256_digest":    hex.EncodeToString(sum[:]),
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return f.do(t, http.MethodPost, "/repository/"+repository+"/", buffer.Bytes(), http.Header{"Content-Type": {writer.FormDataContentType()}})
}

func TestPypiHostedUploadAndInstall(t *testing.T) {
	requirePip(t)
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))

	// The project page 404s until something is uploaded.
	if response, _ := f.do(t, http.MethodGet, "/repository/hosted/simple/hello/", nil, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("project page for unpublished project: %d", response.StatusCode)
	}

	content := wheel(t, "hello", "1.0.0")
	filename := "hello-1.0.0-py3-none-any.whl"
	response, body := twineUpload(t, f, "hosted", "hello", "1.0.0", filename, content)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %s", response.StatusCode, body)
	}

	// pip installs it through suxen; a successful download proves the hash the
	// synthesized index advertises matches the stored bytes.
	dest := pipDownload(t, f, "hosted", "hello==1.0.0")
	sum := sha256.Sum256(content)
	if got := downloadedDigest(t, dest, filename); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("downloaded digest = %s, want %s", got, hex.EncodeToString(sum[:]))
	}

	// The JSON project page carries the request host and the stored sha256.
	response, body = f.do(t, http.MethodGet, "/repository/hosted/simple/hello/", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
		"Accept":            {"application/vnd.pypi.simple.v1+json"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("project page: %d %s", response.StatusCode, body)
	}
	var page struct {
		Name  string `json:"name"`
		Files []struct {
			Filename string            `json:"filename"`
			URL      string            `json:"url"`
			Hashes   map[string]string `json:"hashes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Files) != 1 {
		t.Fatalf("files = %s", body)
	}
	if got := page.Files[0].URL; got != "https://mirror.example/repository/hosted/packages/hello/"+filename {
		t.Fatalf("file url = %q", got)
	}
	if got := page.Files[0].Hashes["sha256"]; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q, want %s", got, hex.EncodeToString(sum[:]))
	}

	// The root index lists the project (HTML form when JSON is not requested).
	response, body = f.do(t, http.MethodGet, "/repository/hosted/simple/", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), ">hello</a>") {
		t.Fatalf("root index: %d %s", response.StatusCode, body)
	}
}

func TestPypiProxyDownloadsThroughSuxenAndReplaysFromCache(t *testing.T) {
	requirePip(t)
	f := newFixture(t, time.Hour)
	idx := newIndex(t, map[string][]string{"hello": {"1.0.0", "1.1.0"}}, false)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "pypi", "format": "pypi", "type": "proxy", "upstream": idx.pages.URL}))

	dest := pipDownload(t, f, "pypi", "hello==1.0.0")
	if got := downloadedDigest(t, dest, "hello-1.0.0-py3-none-any.whl"); got != idx.projects["hello"][0].digest {
		t.Fatalf("downloaded digest = %s", got)
	}
	// One project page, its advertised metadata companion, and one wheel went upstream.
	if hits := idx.hits.Load(); hits != 3 {
		t.Fatalf("upstream hits = %d, want 3", hits)
	}

	idx.blocked.Store(true)
	replay := pipDownload(t, f, "pypi", "hello==1.0.0")
	if got := downloadedDigest(t, replay, "hello-1.0.0-py3-none-any.whl"); got != idx.projects["hello"][0].digest {
		t.Fatalf("replayed digest = %s", got)
	}

	response, body := f.do(t, http.MethodGet, "/api/v1/repositories/pypi/assets", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list assets: %d %s", response.StatusCode, body)
	}
	for _, want := range []string{`"name":"hello"`, `"version":"1.0.0"`, `"simple/hello"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("asset listing lacks %s:\n%s", want, body)
		}
	}
}

func TestPypiHTMLPageRewriteAndMetadataResolution(t *testing.T) {
	f := newFixture(t, time.Hour)
	idx := newIndex(t, map[string][]string{"hello": {"1.0.0"}}, true)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "pypi", "format": "pypi", "type": "proxy", "upstream": idx.pages.URL}))
	dist := idx.projects["hello"][0]
	filesHost := strings.TrimPrefix(idx.files.URL, "http://")

	response, body := f.do(t, http.MethodGet, "/repository/pypi/simple/hello/", nil, http.Header{
		"Host":              {"mirror.example"},
		"X-Forwarded-Proto": {"https"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("page: %d %s", response.StatusCode, body)
	}
	want := `href="https://mirror.example/repository/pypi/files/http/` + filesHost + `/packages/` + dist.digest + `/` + dist.filename + `#sha256=` + dist.digest + `" data-requires-python="&gt;=3.8"`
	if !strings.Contains(string(body), want) {
		t.Fatalf("rewritten page lacks %s:\n%s", want, body)
	}

	response, body = f.do(t, http.MethodGet, "/repository/pypi/files/http/"+filesHost+"/packages/"+dist.digest+"/"+dist.filename+".metadata", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Name: hello") {
		t.Fatalf("metadata companion: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/pypi/files/http/"+filesHost+"/packages/"+dist.digest+"/"+dist.filename, nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, dist.content) {
		t.Fatalf("wheel: %d (%d bytes)", response.StatusCode, len(body))
	}

	response, body = f.do(t, http.MethodGet, "/repository/pypi/simple/", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `href="`+f.suxen.URL+`/repository/pypi/simple/hello/"`) {
		t.Fatalf("root index: %d %s", response.StatusCode, body)
	}
}

func TestPypiGroupMergesPages(t *testing.T) {
	f := newFixture(t, time.Hour)
	first := newIndex(t, map[string][]string{"hello": {"1.0.0"}}, false)
	second := newIndex(t, map[string][]string{"hello": {"1.2.0"}}, true)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "first", "format": "pypi", "type": "proxy", "upstream": first.pages.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "second", "format": "pypi", "type": "proxy", "upstream": second.pages.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "all", "format": "pypi", "type": "group", "members": []string{"first", "second"}}))

	response, body := f.do(t, http.MethodGet, "/repository/all/simple/hello/", nil, http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("group page: %d %s", response.StatusCode, body)
	}
	var document struct {
		Files []struct {
			Filename string `json:"filename"`
			URL      string `json:"url"`
		} `json:"files"`
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("group page is not JSON: %v\n%s", err, body)
	}
	if len(document.Files) != 2 || len(document.Versions) != 2 {
		t.Fatalf("merged page = %s", body)
	}
	for _, file := range document.Files {
		if !strings.HasPrefix(file.URL, f.suxen.URL+"/repository/all/files/http/") {
			t.Fatalf("group file url = %q", file.URL)
		}
	}
	secondDist := second.projects["hello"][0]
	response, body = f.do(t, http.MethodGet, strings.TrimPrefix(document.Files[1].URL, f.suxen.URL), nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, secondDist.content) {
		t.Fatalf("group wheel via member: %d (%d bytes)", response.StatusCode, len(body))
	}

	if status := f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}); status != http.StatusCreated {
		t.Fatalf("hosted pypi repository rejected: %d", status)
	}
}
