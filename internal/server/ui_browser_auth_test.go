//go:build !noui

package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// This drives the embedded UI in Chromium against the real API, rather than
// replacing fetch or asserting only that the anonymous shell rendered.
func TestAuthenticatedAdministrationUIBrowserCRUD(t *testing.T) {
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	probe := fmt.Sprintf(`
const waitFor = async (predicate, label) => {
  if (predicate()) return;
  await new Promise((resolve) => {
    const observer = new MutationObserver(() => {
      if (predicate()) {
        observer.disconnect();
        resolve();
      }
    });
    observer.observe(document, {subtree: true, childList: true, characterData: true, attributes: true});
    window.addEventListener("hashchange", () => {
      if (predicate()) {
        observer.disconnect();
        resolve();
      }
    }, {once: true});
  });
};
const ui = () => document;
const button = (label) => [...document.querySelectorAll("#app-view button")]
  .find((candidate) => candidate.textContent.trim() === label);
(async () => {
  await waitFor(() => ui()?.querySelector("#identity-summary")?.textContent === "Anonymous session", "anonymous identity");
  ui().querySelector("#token-input").value = %q;
  ui().querySelector("#token-login").requestSubmit();
  await waitFor(() => ui().querySelector("#identity-summary").textContent.includes("admin · api-token"), "token identity");
  await waitFor(() => button("Create repository"), "repository list");
  button("Create repository").click();
  await waitFor(() => ui().querySelector("#resource-dialog").open, "create dialog");
  const name = ui().querySelector("#resource-fields input");
  name.value = "browsercrud";
  name.dispatchEvent(new Event("input", {bubbles: true}));
  ui().querySelector("#resource-form").requestSubmit();
  await waitFor(() => window.location.hash.includes("browsercrud") && button("Edit"), "created repository detail");
  button("Edit").click();
  await waitFor(() => ui().querySelector("#resource-dialog").open, "edit dialog");
  ui().querySelector("#resource-form").requestSubmit();
  await waitFor(() => !ui().querySelector("#resource-dialog").open &&
    ui().querySelector("#notice-message").textContent.includes("was saved"), "repository update");
  await waitFor(() => button("Delete"), "delete action");
  button("Delete").click();
  await waitFor(() => ui().querySelector("#confirm-dialog").open, "delete confirmation");
  ui().querySelector("#confirm-action").click();
  await waitFor(() => window.location.hash === "#/repositories", "repository deletion");
  const deleted = await fetch("/api/v1/repositories/browsercrud", {
    headers: {Authorization: "Bearer " + %q},
  });
  if (deleted.status !== 404) throw new Error("deleted repository status " + deleted.status);
  ui().querySelector("#logout").click();
  await waitFor(() => ui().querySelector("#identity-summary").textContent === "Anonymous session", "logout");
  if (sessionStorage.getItem("suxenToken")) throw new Error("token remained in session storage");
  document.body.dataset.result = "passed";
  await fetch("/browser-auth-probe-result?status=passed");
})().catch((error) => {
  document.body.dataset.result = "failed";
  document.body.dataset.error = error.message;
  fetch("/browser-auth-probe-result?status=failed&error=" + encodeURIComponent(error.message));
});`, testToken, testToken)
	result := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browser-auth-probe-result":
			select {
			case result <- r.URL.Query().Get("status") + ": " + r.URL.Query().Get("error"):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
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
			page := strings.Replace(recorded.Body.String(), "</body>", `<script src="/browser-auth-probe.js"></script></body>`, 1)
			_, _ = io.WriteString(w, page)
		case "/browser-auth-probe.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = io.WriteString(w, probe)
		default:
			fixture.Handler.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	if got := runBrowserUntilResult(t, browser, server.URL+"/ui/#/repositories", result); got != "passed: " {
		t.Fatalf("browser did not complete authenticated CRUD: %s", got)
	}
}

func TestAdministrationUIBrowserCookieCSRF(t *testing.T) {
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	statuses := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/csrf-seed" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<body data-result="pending"><script src="/csrf-seed.js"></script></body>`)
			return
		}
		if r.URL.Path == "/csrf-seed.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = io.WriteString(w, `document.cookie = "suxen_oidc_token=invalid; path=/; SameSite=Lax";
fetch("/auth/oidc/test/logout", {method: "POST"}).then((response) => {
  if (response.status !== 204) throw new Error("same-origin status " + response.status);
  document.cookie = "suxen_oidc_token=invalid; path=/; SameSite=Lax";
  location.assign(new URLSearchParams(location.search).get("next"));
}).catch((error) => { document.body.dataset.result = error.message; });`)
			return
		}
		if r.URL.Path == "/auth/oidc/test/logout" && r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host {
			recorded := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(recorded, r)
			select {
			case statuses <- fmt.Sprint(recorded.Code):
			default:
			}
			for name, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		fixture.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	crossOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<body data-result="pending"><script>
fetch(%q, {method: "POST", mode: "no-cors", credentials: "include"})
  .then(() => { document.body.dataset.result = "sent"; });
</script></body>`, server.URL+"/auth/oidc/test/logout")
	}))
	defer crossOrigin.Close()
	// A query parameter is read by the seed page, avoiding an inline script in
	// the UI origin while preserving the real browser's automatic Origin header.
	seed := url.QueryEscape(crossOrigin.URL)
	if got := runBrowserUntilResult(t, browser, server.URL+"/csrf-seed?next="+seed, statuses); got != "403" {
		t.Fatalf("cross-origin cookie mutation status = %s, want 403", got)
	}
}

func TestAdministrationUIBrowserOIDCCallbackAndLogout(t *testing.T) {
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const providerName = "browser-idp"
	const clientID = "suxen-browser"
	var issuer string
	var challenge, nonce string
	var authorizationMu sync.Mutex
	probe := `
const waitFor = async (predicate) => {
  if (predicate()) return;
  await new Promise((resolve) => {
    const observer = new MutationObserver(() => {
      if (predicate()) { observer.disconnect(); resolve(); }
    });
    observer.observe(document, {subtree: true, childList: true, characterData: true, attributes: true});
  });
};
(async () => {
  if (!sessionStorage.getItem("oidcBrowserProbeStarted")) {
    await waitFor(() => document.querySelector("#identity-summary")?.textContent === "Anonymous session");
    await waitFor(() => document.querySelector("#login-providers button"));
    sessionStorage.setItem("oidcBrowserProbeStarted", "1");
    document.querySelector("#login-providers button").click();
    return;
  }
  await waitFor(() => document.querySelector("#identity-summary")?.textContent.includes("release-bot · oidc-session"));
  const whoami = await (await fetch("/api/v1/whoami")).json();
  if (whoami.authenticationKind !== "oidc-session" || whoami.sessionProvider !== "browser-idp") {
    throw new Error("OIDC callback did not establish the expected cookie session");
  }
  document.querySelector("#logout").click();
  await waitFor(() => document.querySelector("#identity-summary")?.textContent === "Anonymous session");
  const after = await (await fetch("/api/v1/whoami")).json();
  if (after.authenticated) throw new Error("OIDC logout did not clear the cookie session");
  document.body.dataset.result = "passed";
  await fetch("/browser-oidc-probe-result?status=passed");
})().catch((error) => {
  document.body.dataset.result = "failed";
  document.body.dataset.error = error.message;
  fetch("/browser-oidc-probe-result?status=failed&error=" + encodeURIComponent(error.message));
});`
	result := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browser-oidc-probe-result":
			select {
			case result <- r.URL.Query().Get("status") + ": " + r.URL.Query().Get("error"):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case "/idp/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
			})
		case "/idp/authorize":
			if r.URL.Query().Get("code_challenge_method") != "S256" || r.URL.Query().Get("state") == "" {
				http.Error(w, "invalid authorization request", http.StatusBadRequest)
				return
			}
			authorizationMu.Lock()
			challenge = r.URL.Query().Get("code_challenge")
			nonce = r.URL.Query().Get("nonce")
			authorizationMu.Unlock()
			callback, err := url.Parse(r.URL.Query().Get("redirect_uri"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			query := callback.Query()
			query.Set("code", "browser-authorization-code")
			query.Set("state", r.URL.Query().Get("state"))
			callback.RawQuery = query.Encode()
			http.Redirect(w, r, callback.String(), http.StatusFound)
		case "/idp/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			verifierHash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			authorizationMu.Lock()
			valid := r.Form.Get("code") == "browser-authorization-code" &&
				base64.RawURLEncoding.EncodeToString(verifierHash[:]) == challenge && nonce != ""
			issuedNonce := nonce
			authorizationMu.Unlock()
			if !valid {
				http.Error(w, "invalid authorization code or PKCE verifier", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "browser-access-token", "token_type": "Bearer", "expires_in": 3600,
				"id_token": signTestIDTokenForIssuer(t, privateKey, issuer, clientID, nil, issuedNonce),
			})
		case "/idp/keys":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(testJWKS(&privateKey.PublicKey))
		case "/browser-oidc-probe.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = io.WriteString(w, probe)
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
			page := strings.Replace(recorded.Body.String(), "</body>", `<script src="/browser-oidc-probe.js"></script></body>`, 1)
			_, _ = io.WriteString(w, page)
		default:
			fixture.Handler.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	issuer = server.URL + "/idp"
	fixture.Handler.identity.SetHTTPClient(&http.Client{Timeout: 3 * time.Second})
	if err := fixture.Metadata.CreateOIDCProvider(context.Background(), domain.OIDCProvider{
		Name: providerName, Issuer: issuer, ClientID: clientID, ClientSecret: "browser-client-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if got := runBrowserUntilResult(t, browser, server.URL+"/ui/#/repositories", result); got != "passed: " {
		t.Fatalf("browser did not complete OIDC callback and logout: %s", got)
	}
}

func runBrowserUntilResult(t *testing.T, browser, target string, result <-chan string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, browser,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--remote-debugging-port=0", "--user-data-dir="+filepath.Join(t.TempDir(), "profile"),
		target,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	var value string
	var exitErr error
	finished := false
	select {
	case value = <-result:
	case exitErr = <-exited:
		finished = true
	case <-ctx.Done():
	}
	cancel()
	if !finished {
		select {
		case exitErr = <-exited:
		case <-time.After(3 * time.Second):
			t.Fatal("browser process did not exit after cancellation")
		}
	}
	if value == "" {
		t.Fatalf("browser ended before reporting a result: %v, %v", ctx.Err(), exitErr)
	}
	return value
}
