package pypi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestEmptyPyPIProxyAndGroupsRemainConsumable(t *testing.T) {
	f := newFixture(t, time.Hour)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		switch strings.TrimSuffix(r.URL.Path, "/") {
		case "/simple":
			fmt.Fprint(w, `{"projects":[],"meta":{"api-version":"1.0"},"extra":{"keep":true}}`)
		case "/simple/empty":
			fmt.Fprint(w, `{"name":"empty","files":[],"meta":{"api-version":"1.0"},"extra":{"keep":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(empty.Close)
	filled := newIndex(t, map[string][]string{"empty": {"1.0.0"}}, false)
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "empty", "format": "pypi", "type": "proxy", "upstream": empty.URL,
	}))
	mustCreate(t, f.createRepository(t, map[string]any{
		"name": "filled", "format": "pypi", "type": "proxy", "upstream": filled.pages.URL,
	}))
	for name, members := range map[string][]string{
		"empty-group": {"empty"},
		"mixed-group": {"empty", "filled"},
	} {
		mustCreate(t, f.createRepository(t, map[string]any{
			"name": name, "format": "pypi", "type": "group", "members": members,
		}))
	}
	for _, test := range []struct {
		repository string
		wantFiles  int
	}{
		{"empty", 0}, {"empty-group", 0}, {"mixed-group", 1},
	} {
		response, body := f.do(t, http.MethodGet,
			"/repository/"+test.repository+"/simple/empty/", nil,
			http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}},
		)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", test.repository, response.StatusCode, body)
		}
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		files, ok := document["files"].([]any)
		if !ok || len(files) != test.wantFiles {
			t.Fatalf("%s files = %s", test.repository, body)
		}
	}
	response, rootBody := f.do(t, http.MethodGet, "/repository/empty/simple/", nil,
		http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("root index: %d %s", response.StatusCode, rootBody)
	}
	var root map[string]any
	if err := json.Unmarshal(rootBody, &root); err != nil {
		t.Fatal(err)
	}
	if projects, ok := root["projects"].([]any); !ok || len(projects) != 0 {
		t.Fatalf("root projects = %s", rootBody)
	}

	// pip must report an unavailable distribution normally, rather than crash
	// while iterating a JSON null from the proxy's empty project page.
	requirePip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-m", "pip", "download", "empty",
		"--index-url", f.suxen.URL+"/repository/empty/simple/",
		"--dest", t.TempDir(), "--no-deps", "--no-cache-dir",
		"--disable-pip-version-check", "--isolated",
	)
	command.Env = append(os.Environ(), "PIP_NO_INPUT=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "No matching distribution found") ||
		strings.Contains(string(output), "Traceback") || strings.Contains(string(output), "TypeError") {
		t.Fatalf("pip did not handle empty project normally: %v, %v\n%s", err, ctx.Err(), output)
	}
}
