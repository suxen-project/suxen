//go:build !noui

package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
)

func TestEmbeddedAdministrationUI(t *testing.T) {
	t.Parallel()
	if !uiEnabled {
		t.Fatal("default server build does not contain the administration UI")
	}
	fixture := newServerFixture(t)

	index := fixture.request(t, http.MethodGet, "/", nil, false)
	assertStatus(t, index, http.StatusOK)
	if index.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("unexpected index content type %q", index.Header.Get("Content-Type"))
	}
	if !strings.Contains(index.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("administration UI is missing its restrictive content security policy")
	}
	body, err := io.ReadAll(index.Body)
	if err != nil {
		t.Fatal(err)
	}
	index.Body.Close()
	if !strings.Contains(string(body), "suxen repository manager") {
		t.Fatal("administration UI index was not embedded")
	}

	modules := []string{
		"api.js",
		"app.js",
		"components.js",
		"defaults_ui.js",
		"dialog.js",
		"navigation.js",
		"operations_ui.js",
		"pagination.js",
		"repositories.js",
		"resource_views.js",
		"route_shapes.js",
		"router.js",
		"schemas.js",
		"session.js",
	}
	for _, module := range modules {
		script := fixture.request(t, http.MethodGet, "/ui/"+module, nil, false)
		assertStatus(t, script, http.StatusOK)
		if script.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Fatalf(
				"unexpected content type %q for %s",
				script.Header.Get("Content-Type"),
				module,
			)
		}
		if script.ContentLength <= 0 {
			t.Fatalf("embedded module %s is empty", module)
		}
		script.Body.Close()
	}

	head := fixture.request(t, http.MethodHead, "/ui/styles.css", nil, false)
	assertStatus(t, head, http.StatusOK)
	if head.ContentLength <= 0 {
		t.Fatal("UI HEAD response did not report the embedded asset length")
	}
	head.Body.Close()

	missing := fixture.request(t, http.MethodGet, "/ui/missing.js", nil, false)
	assertStatus(t, missing, http.StatusNotFound)
	missing.Body.Close()

	post := fixture.request(t, http.MethodPost, "/ui/app.js", nil, false)
	assertStatus(t, post, http.StatusMethodNotAllowed)
	post.Body.Close()
}

func TestAdministrationUIRendersInBrowser(t *testing.T) {
	t.Parallel()
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	server := httptest.NewServer(fixture.Handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile := filepath.Join(t.TempDir(), "profile")
	command := exec.CommandContext(ctx, browser,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--virtual-time-budget=3000", "--user-data-dir="+profile, "--dump-dom",
		server.URL+"/ui/#/overview",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run browser: %v\n%s", err, output)
	}
	rendered := string(output)
	for _, expected := range []string{"Anonymous session", "Repository operations", "Storage statistics require admin:stats:read."} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("browser DOM did not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestArtifactScriptsCannotExecuteInAdministrationOrigin(t *testing.T) {
	t.Parallel()
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	artifacts := []struct {
		path        string
		contentType string
		body        string
	}{
		{
			path:        "explicit.html",
			contentType: "text/html",
			body:        `<script>parent.document.body.dataset.artifactExecuted = "html"</script>`,
		},
		{
			path:        "image.svg",
			contentType: "image/svg+xml",
			body:        `<svg xmlns="http://www.w3.org/2000/svg" onload="parent.document.body.dataset.artifactExecuted='svg'"/>`,
		},
	}
	for _, artifact := range artifacts {
		upload := fixture.requestWithContentType(
			t,
			http.MethodPut,
			"/repository/raw/browser/"+artifact.path,
			[]byte(artifact.body),
			artifact.contentType,
			true,
		)
		assertStatus(t, upload, http.StatusCreated)
		upload.Body.Close()
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/artifact-isolation-probe" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><body data-probe="ready">
<iframe src="/repository/raw/browser/explicit.html"></iframe>
<iframe src="/repository/raw/browser/image.svg"></iframe>
<script>setTimeout(() => document.querySelectorAll("iframe").forEach((frame) => frame.remove()), 1000)</script>
</body>`)
			return
		}
		fixture.Handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// Chromium keeps a download job alive for attachment responses even after
	// it has emitted --dump-dom. Bound that expected wait and inspect only the
	// rendered DOM on stdout.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	profile := filepath.Join(t.TempDir(), "profile")
	command := exec.CommandContext(ctx, browser,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--virtual-time-budget=3000", "--user-data-dir="+profile, "--dump-dom",
		server.URL+"/artifact-isolation-probe",
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	err := command.Run()
	if err != nil && ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("run browser: %v\n%s", err, output.String())
	}
	rendered := output.String()
	if !strings.Contains(rendered, `data-probe="ready"`) {
		t.Fatalf("browser did not render the isolation probe:\n%s", rendered)
	}
	if strings.Contains(rendered, "data-artifact-executed") {
		t.Fatalf("artifact script executed with administration-origin access:\n%s", rendered)
	}
}

func chromiumExecutable(t *testing.T) string {
	t.Helper()
	browser := os.Getenv("CHROMIUM")
	if browser == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if resolved, err := exec.LookPath(candidate); err == nil {
				browser = resolved
				break
			}
		}
	}
	if browser == "" {
		if os.Getenv("SUXEN_REQUIRE_BROWSER_TEST") == "1" {
			t.Fatal("required Chromium browser is not installed")
		}
		t.Skip("Chromium browser is not installed")
	}
	return browser
}

func TestAdministrationUIUsesSafeRoutedInteractionPatterns(t *testing.T) {
	index, err := adminUI.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(index)
	for _, required := range []string{
		`href="#/repositories"`,
		`href="#/search"`,
		`id="login-providers"`,
		`id="token-login"`,
		`id="token-input"`,
		`id="resource-dialog"`,
		`id="confirm-dialog"`,
		`id="secret-dialog"`,
		`aria-labelledby="resource-dialog-title"`,
		`aria-describedby="confirm-message"`,
		`aria-labelledby="secret-title"`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("administration index is missing %q", required)
		}
	}
	if strings.Contains(page, "onclick=") {
		t.Fatal("administration index contains an inline event handler")
	}

	entries, err := adminUI.ReadDir("ui")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		source, readErr := adminUI.ReadFile("ui/" + entry.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, forbidden := range []string{"innerHTML", "window.prompt", "window.confirm"} {
			if strings.Contains(string(source), forbidden) {
				t.Errorf("%s contains forbidden interaction %q", entry.Name(), forbidden)
			}
		}
	}

	components, err := adminUI.ReadFile("ui/components.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, accessibleName := range []string{
		`descriptor.label`,
		`key ${index + 1}`,
		`value ${index + 1}`,
		`row ${index + 1}`,
	} {
		if !strings.Contains(string(components), accessibleName) {
			t.Errorf("compound form controls are missing accessible name source %q", accessibleName)
		}
	}

	resourceViews, err := adminUI.ReadFile("ui/resource_views.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(resourceViews),
		"showSecret(`Token for ${username}`, created.token);\n      refreshCurrentRoute();",
	) {
		t.Fatal("token creation does not invalidate and refresh its collection")
	}
}

func TestRuntimeDisabledAdministrationUIRoutesReturnNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.DisableUI = true })

	for _, requestPath := range []string{"/", "/ui", "/ui/", "/ui/app.js"} {
		response := fixture.request(t, http.MethodGet, requestPath, nil, false)
		assertStatus(t, response, http.StatusNotFound)
		response.Body.Close()
	}

	health := fixture.request(t, http.MethodGet, "/healthz", nil, false)
	assertStatus(t, health, http.StatusOK)
	health.Body.Close()
}
