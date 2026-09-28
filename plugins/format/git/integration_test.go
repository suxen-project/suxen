package git_test

import (
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
	"github.com/suxen-project/suxen/internal/server"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/git"
)

const (
	adminToken    = "git-integration-admin-token"
	adminUser     = "admin"
	adminPassword = "integration-password"
)

// The integration tests drive a real git client against a suxen instance
// whose git proxy points at an upstream served by `git upload-pack`, the same
// process git-http-backend runs. They are the oracle for the snapshot model:
// a depth-1 clone, a full clone ending up shallow, cache hits with the
// upstream blocked, and the refusals.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

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
		MaxUploadBytes:    64 << 20,
		ProxyManifestTTL:  proxyTTL,
		OutboundTimeout:   10 * time.Second,
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
	suxen := httptest.NewServer(handler)
	t.Cleanup(suxen.Close)
	return &fixture{handler: handler, suxen: suxen}
}

func (f *fixture) api(t *testing.T, method, requestPath string, payload any) (*http.Response, []byte) {
	t.Helper()
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = encoded
	}
	request, err := http.NewRequest(method, f.suxen.URL+requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
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

func (f *fixture) createGitProxy(t *testing.T, name, upstream string) {
	t.Helper()
	response, body := f.api(t, http.MethodPost, "/api/v1/repositories", map[string]any{
		"name":     name,
		"format":   "git",
		"type":     "proxy",
		"upstream": upstream,
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create repository: %d %s", response.StatusCode, body)
	}
}

// cloneURL embeds the admin credentials so the git client never prompts.
func (f *fixture) cloneURL(t *testing.T, repository, repoPath string) string {
	t.Helper()
	parsed, err := url.Parse(f.suxen.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(adminUser, adminPassword)
	return parsed.String() + "/repository/" + repository + "/" + repoPath
}

// upstream serves a local repository the way git-http-backend does, so the
// proxy talks to a genuine `git upload-pack`.
type upstream struct {
	server  *httptest.Server
	repo    string
	hits    atomic.Int64
	blocked atomic.Bool
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{repo: t.TempDir()}
	runGit(t, u.repo, "init", "-q", "-b", "main")
	writeFile(t, u.repo, "README.md", "first\n")
	runGit(t, u.repo, "add", ".")
	runGit(t, u.repo, "commit", "-q", "-m", "first")
	writeFile(t, u.repo, "second.txt", "second\n")
	runGit(t, u.repo, "add", ".")
	runGit(t, u.repo, "commit", "-q", "-m", "second")
	runGit(t, u.repo, "tag", "v1")
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	if u.blocked.Load() {
		http.Error(w, "upstream blocked", http.StatusServiceUnavailable)
		return
	}
	environment := append(os.Environ(), "GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	switch {
	case strings.HasSuffix(r.URL.Path, "/info/refs"):
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "001e# service=git-upload-pack\n0000")
		command := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", u.repo)
		command.Env = environment
		output, err := command.Output()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(output)
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		command := exec.Command("git", "upload-pack", "--stateless-rpc", u.repo)
		command.Env = environment
		command.Stdin = r.Body
		command.Stdout = w
		_ = command.Run()
	default:
		http.NotFound(w, r)
	}
}

func (u *upstream) head(t *testing.T, ref string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, u.repo, "rev-parse", ref))
}

func writeFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitEnvironment isolates the client from the developer's configuration and
// credential helpers and makes any credential prompt fail instead of hang.
func gitEnvironment(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=suxen test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=suxen test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := tryGit(t, directory, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func tryGit(t *testing.T, directory string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = gitEnvironment(t)
	output, err := command.CombinedOutput()
	return string(output), err
}

func assertShallowClone(t *testing.T, clone string, wantHead string) {
	t.Helper()
	if got := strings.TrimSpace(runGit(t, clone, "rev-parse", "HEAD")); got != wantHead {
		t.Fatalf("HEAD = %s, want %s", got, wantHead)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "shallow")); err != nil {
		t.Fatalf("clone is not shallow: %v", err)
	}
	if got := strings.TrimSpace(runGit(t, clone, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("history depth = %s, want 1", got)
	}
	for _, name := range []string{"README.md", "second.txt"} {
		if _, err := os.Stat(filepath.Join(clone, name)); err != nil {
			t.Fatalf("checked-out file %s: %v", name, err)
		}
	}
	runGit(t, clone, "fsck", "--connectivity-only")
}

func TestGitProxyDepthOneCloneAndCacheHit(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	f.createGitProxy(t, "github", up.server.URL)
	head := up.head(t, "HEAD")

	first := filepath.Join(t.TempDir(), "first")
	runGit(t, ".", "clone", "-q", "--depth", "1", f.cloneURL(t, "github", "acme/widget.git"), first)
	assertShallowClone(t, first, head)

	// The proxy performed exactly one advertisement, one ls-refs, and one
	// depth-1 fetch against the upstream.
	if hits := up.hits.Load(); hits != 3 {
		t.Fatalf("upstream hits after first clone = %d, want 3", hits)
	}

	up.blocked.Store(true)
	second := filepath.Join(t.TempDir(), "second")
	runGit(t, ".", "clone", "-q", "--depth", "1", f.cloneURL(t, "github", "acme/widget"), second)
	assertShallowClone(t, second, head)

	response, body := f.api(t, http.MethodGet, "/api/v1/repositories/github/assets", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list assets: %d %s", response.StatusCode, body)
	}
	for _, want := range []string{
		`"acme/widget.git/refs"`,
		`"acme/widget.git/snapshots/` + head + `.pack"`,
		`"commit":"` + head + `"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("asset listing lacks %s:\n%s", want, body)
		}
	}
}

func TestGitProxyAnnotatedTagDepthOneCloneAndCacheHit(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	runGit(t, up.repo, "tag", "-a", "annotated-v1", "-m", "annotated", "HEAD")
	f.createGitProxy(t, "github", up.server.URL)
	tag := strings.TrimSpace(runGit(t, up.repo, "rev-parse", "annotated-v1"))
	head := up.head(t, "HEAD")
	if tag == head {
		t.Fatal("annotated tag and commit unexpectedly have the same object id")
	}
	cloneTag := func(destination string) {
		t.Helper()
		runGit(t, ".", "clone", "-q", "--depth", "1", "--single-branch", "--branch", "annotated-v1",
			f.cloneURL(t, "github", "acme/widget.git"), destination)
		assertShallowClone(t, destination, head)
		if objectType := strings.TrimSpace(runGit(t, destination, "cat-file", "-t", tag)); objectType != "tag" {
			t.Fatalf("selected tag object type = %q", objectType)
		}
	}
	cloneTag(filepath.Join(t.TempDir(), "first"))
	up.blocked.Store(true)
	cloneTag(filepath.Join(t.TempDir(), "cached"))
}

func TestGitProxyAnnotatedRootTagClone(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	root := strings.TrimSpace(runGit(t, up.repo, "rev-list", "--max-parents=0", "HEAD"))
	runGit(t, up.repo, "tag", "-a", "root-v1", "-m", "root release", root)
	f.createGitProxy(t, "github", up.server.URL)
	clone := filepath.Join(t.TempDir(), "root-tag")
	runGit(t, ".", "clone", "-q", "--depth", "1", "--single-branch", "--branch", "root-v1",
		f.cloneURL(t, "github", "acme/widget.git"), clone)
	if got := strings.TrimSpace(runGit(t, clone, "rev-parse", "HEAD")); got != root {
		t.Fatalf("root tag HEAD = %s, want %s", got, root)
	}
	if got := strings.TrimSpace(runGit(t, clone, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("root tag history depth = %s", got)
	}
	runGit(t, clone, "fsck", "--connectivity-only")
}

func TestGitProxyCachedAnnotatedTagSurvivesRefMove(t *testing.T) {
	requireGit(t)
	const ttl = 150 * time.Millisecond
	f := newFixture(t, ttl)
	up := newUpstream(t)
	runGit(t, up.repo, "tag", "-a", "release", "-m", "first release", "HEAD")
	oldTag := strings.TrimSpace(runGit(t, up.repo, "rev-parse", "release"))
	oldCommit := up.head(t, "HEAD")
	f.createGitProxy(t, "github", up.server.URL)
	remote := f.cloneURL(t, "github", "acme/widget.git")
	first := filepath.Join(t.TempDir(), "first")
	runGit(t, ".", "clone", "-q", "--depth", "1", "--single-branch", "--branch", "release", remote, first)

	writeFile(t, up.repo, "moved.txt", "moved\n")
	runGit(t, up.repo, "add", ".")
	runGit(t, up.repo, "commit", "-q", "-m", "move release")
	runGit(t, up.repo, "tag", "-f", "-a", "release", "-m", "second release", "HEAD")
	if got := strings.TrimSpace(runGit(t, up.repo, "rev-parse", "release")); got == oldTag {
		t.Fatal("annotated tag did not move")
	}
	time.Sleep(2 * ttl)
	runGit(t, ".", "ls-remote", remote) // refresh the mutable ref list

	// The old tag is no longer advertised, but its immutable pack remains.
	// Fetching its exact object ID must use the boundary stored with that pack.
	checkout := filepath.Join(t.TempDir(), "old-tag")
	runGit(t, ".", "init", "-q", checkout)
	runGit(t, checkout, "fetch", "-q", "--depth", "1", remote, oldTag)
	if got := strings.TrimSpace(runGit(t, checkout, "rev-parse", "FETCH_HEAD^{commit}")); got != oldCommit {
		t.Fatalf("cached old tag resolves to %s, want %s", got, oldCommit)
	}
}

func TestGitProxyFullCloneEndsUpShallow(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	f.createGitProxy(t, "github", up.server.URL)

	// main and v1 point at the same commit, so a full clone wants one
	// snapshot and the client accepts the server-declared shallow boundary.
	clone := filepath.Join(t.TempDir(), "full")
	runGit(t, ".", "clone", "-q", f.cloneURL(t, "github", "acme/widget.git"), clone)
	assertShallowClone(t, clone, up.head(t, "HEAD"))
	tags := runGit(t, clone, "tag", "--list")
	if !strings.Contains(tags, "v1") {
		t.Fatalf("tags = %q, want v1", tags)
	}
}

func TestGitProxyRefusesMultipleWants(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	runGit(t, up.repo, "checkout", "-q", "-b", "feature")
	writeFile(t, up.repo, "feature.txt", "feature\n")
	runGit(t, up.repo, "add", ".")
	runGit(t, up.repo, "commit", "-q", "-m", "feature")
	runGit(t, up.repo, "checkout", "-q", "main")
	f.createGitProxy(t, "github", up.server.URL)

	output, err := tryGit(t, ".", "clone", "-q", f.cloneURL(t, "github", "acme/widget.git"), filepath.Join(t.TempDir(), "all"))
	if err == nil {
		t.Fatal("full clone of a multi-branch repository succeeded")
	}
	if !strings.Contains(output, "one snapshot per fetch") {
		t.Fatalf("clone error lacks guidance:\n%s", output)
	}

	branch := filepath.Join(t.TempDir(), "feature")
	runGit(t, ".", "clone", "-q", "--branch", "feature", "--single-branch", f.cloneURL(t, "github", "acme/widget.git"), branch)
	if got := strings.TrimSpace(runGit(t, branch, "rev-parse", "HEAD")); got != up.head(t, "feature") {
		t.Fatalf("feature HEAD = %s", got)
	}
}

func TestGitProxyRevalidatesRefsOnTTL(t *testing.T) {
	requireGit(t)
	ttl := 150 * time.Millisecond
	f := newFixture(t, ttl)
	up := newUpstream(t)
	f.createGitProxy(t, "github", up.server.URL)

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, ".", "clone", "-q", "--depth", "1", f.cloneURL(t, "github", "acme/widget.git"), clone)

	writeFile(t, up.repo, "third.txt", "third\n")
	runGit(t, up.repo, "add", ".")
	runGit(t, up.repo, "commit", "-q", "-m", "third")
	moved := up.head(t, "HEAD")
	time.Sleep(2 * ttl)

	runGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	if got := strings.TrimSpace(runGit(t, clone, "rev-parse", "origin/main")); got != moved {
		t.Fatalf("origin/main = %s, want %s", got, moved)
	}
	runGit(t, clone, "checkout", "-q", "origin/main")
	if _, err := os.Stat(filepath.Join(clone, "third.txt")); err != nil {
		t.Fatalf("updated snapshot not checked out: %v", err)
	}
}

func TestGitProxyRefusesPushAndNonProxies(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	f.createGitProxy(t, "github", up.server.URL)

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, ".", "clone", "-q", "--depth", "1", f.cloneURL(t, "github", "acme/widget.git"), clone)
	writeFile(t, clone, "local.txt", "local\n")
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-q", "-m", "local")
	output, err := tryGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatal("push succeeded")
	}
	if !strings.Contains(output, "pushes are not accepted") {
		t.Fatalf("push error lacks the read-only message:\n%s", output)
	}

	for _, spec := range []map[string]any{
		{"name": "hosted", "format": "git", "type": "hosted"},
		{"name": "group", "format": "git", "type": "group", "members": []string{"github"}},
		{"name": "configured", "format": "git", "type": "proxy", "upstream": up.server.URL, "formatConfig": map[string]any{"depth": 2}},
	} {
		response, body := f.api(t, http.MethodPost, "/api/v1/repositories", spec)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d %s", spec["name"], response.StatusCode, body)
		}
	}
}

func TestGitProxyReportsUpstreamFailure(t *testing.T) {
	requireGit(t)
	f := newFixture(t, time.Hour)
	up := newUpstream(t)
	f.createGitProxy(t, "github", up.server.URL)
	up.blocked.Store(true)

	output, err := tryGit(t, ".", "clone", "-q", "--depth", "1", f.cloneURL(t, "github", "acme/widget.git"), filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("clone with a failing upstream succeeded")
	}
	if !strings.Contains(output, "snapshot temporarily unavailable") || strings.Contains(output, "upstream blocked") {
		t.Fatalf("clone error does not preserve the generic failure boundary:\n%s", output)
	}
}
