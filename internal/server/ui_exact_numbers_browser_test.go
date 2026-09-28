//go:build !noui

package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise the actual embedded forms and API transport in a browser: backend
// precision is insufficient if an unrelated form edit changes a policy literal.
func TestAdministrationUIBrowserExactPolicyNumbers(t *testing.T) {
	browser := chromiumExecutable(t)
	fixture := newServerFixture(t)
	probe := fmt.Sprintf(`
import {api} from "/ui/api.js";
import {openForm} from "/ui/components.js";
import {schemas} from "/ui/schemas.js";
const token = %q;
const endpoint = "/api/v1/repositories/raw/download-gate";
const expected = '"value":[9007199254740993,0.12345678901234567890123456789,1e1000]';
const original = '{"criteria":[{"path":"scan.serial","op":"in",' + expected + '}],"enabled":true,"inheritGlobal":true}';
const toggle = (details, open) => new Promise((resolve) => {
  details.addEventListener("toggle", resolve, {once: true});
  details.open = open;
});
(async () => {
  api.setToken(token);
  for (const mode of ["regular", "advanced"]) {
    const seeded = await fetch(endpoint, {method: "PUT", body: original,
      headers: {Authorization: "Bearer " + token, "Content-Type": "application/json"}});
    if (!seeded.ok) throw new Error("seed: " + await seeded.text());
    const value = await api.json(endpoint);
    delete value.repository;
    delete value.updatedAt;
    delete value.managed;
    let saved;
    const completed = new Promise((resolve, reject) => { saved = {resolve, reject}; });
    openForm({title: "Exact numeric gate", schema: schemas.downloadGate, value,
      onSave: async (request) => {
        try {
          await api.json(endpoint, {method: "PUT", body: request});
          saved.resolve();
        } catch (error) { saved.reject(error); throw error; }
      }});
    if (mode === "regular") {
      const enabled = document.querySelector('[aria-label="Enabled"]');
      enabled.checked = false;
      enabled.dispatchEvent(new Event("change", {bubbles: true}));
    } else {
      const details = document.querySelector("#advanced-editor");
      await toggle(details, true);
      const editor = document.querySelector("#advanced-json");
      if (!editor.value.includes("9007199254740993") || !editor.value.includes("1e1000")) {
        throw new Error("advanced editor changed numeric text: " + editor.value);
      }
      editor.value = editor.value.replace(/"enabled":\s*true/, '"enabled": false');
      await toggle(details, false);
    }
    document.querySelector("#resource-form").requestSubmit();
    await completed;
    // Let the form's submit handler close the dialog before the next case.
    await new Promise((resolve) => setTimeout(resolve, 0));
    const response = await fetch(endpoint, {headers: {Authorization: "Bearer " + token}});
    const persisted = await response.text();
    if (!response.ok || !persisted.includes(expected) || !persisted.includes('"enabled":false')) {
      throw new Error(mode + " edit changed policy: " + persisted);
    }
  }
  await fetch("/exact-policy-result?status=passed");
})().catch((error) => fetch("/exact-policy-result?status=failed&error=" + encodeURIComponent(error.message)));
`, testToken)
	result := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exact-policy-result":
			select {
			case result <- r.URL.Query().Get("status") + ": " + r.URL.Query().Get("error"):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case "/exact-policy-probe.js":
			w.Header().Set("Content-Type", "text/javascript")
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
			page := strings.Replace(recorded.Body.String(), `src="/ui/app.js"`, `src="/exact-policy-probe.js"`, 1)
			_, _ = io.WriteString(w, page)
		default:
			fixture.Handler.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	if got := runBrowserUntilResult(t, browser, server.URL+"/ui/", result); got != "passed: " {
		t.Fatalf("browser policy round-trip: %s", got)
	}
}
