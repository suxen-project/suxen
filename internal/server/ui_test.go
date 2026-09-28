//go:build !noui

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

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
	result := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browser-render-probe-result":
			select {
			case result <- r.URL.Query().Get("status"):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case "/browser-render-probe.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = io.WriteString(w, `const expected = [
  "Anonymous session", "Repository operations",
  "Storage statistics require admin:stats:read."
];
let reported = false;
const report = (status) => {
  if (reported) return;
  reported = true;
  fetch("/browser-render-probe-result?status=" + encodeURIComponent(status));
};
const missing = () => expected.filter((text) => !document.body.innerText.includes(text));
const observer = new MutationObserver(() => {
  if (missing().length === 0) { observer.disconnect(); report("passed"); }
});
observer.observe(document.body, {subtree: true, childList: true, characterData: true});
if (missing().length === 0) { observer.disconnect(); report("passed"); }
setTimeout(() => report("missing: " + missing().join(", ")), 5000);`)
		case "/ui/":
			recorded := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(recorded, r)
			for name, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(recorded.Code)
			page := strings.Replace(recorded.Body.String(), "</body>", `<script src="/browser-render-probe.js"></script></body>`, 1)
			_, _ = io.WriteString(w, page)
		default:
			fixture.Handler.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if got := runBrowserUntilResult(t, browser, server.URL+"/ui/#/overview", result); got != "passed" {
		t.Fatalf("browser administration UI rendering: %s", got)
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

	result := make(chan string, 1)
	var requested atomic.Uint32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/artifact-isolation-probe-result":
			select {
			case result <- r.URL.Query().Get("status"):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case "/artifact-isolation-probe":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><body data-probe="ready">
<iframe src="/repository/raw/browser/explicit.html"></iframe>
<iframe src="/repository/raw/browser/image.svg"></iframe>
<script>setTimeout(() => {
  const status = document.body.dataset.artifactExecuted ? "failed" : "passed";
  fetch("/artifact-isolation-probe-result?status=" + status);
}, 1500)</script>
</body>`)
		case "/repository/raw/browser/explicit.html":
			requested.Or(1)
			fixture.Handler.ServeHTTP(w, r)
		case "/repository/raw/browser/image.svg":
			requested.Or(2)
			fixture.Handler.ServeHTTP(w, r)
		default:
			fixture.Handler.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if got := runBrowserUntilResult(t, browser, server.URL+"/artifact-isolation-probe", result); got != "passed" {
		t.Fatalf("artifact script executed with administration-origin access: %s", got)
	}
	if got := requested.Load(); got != 3 {
		t.Fatalf("browser fetched artifact types %b, want both HTML and SVG", got)
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
