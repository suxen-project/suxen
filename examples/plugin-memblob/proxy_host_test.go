package memblob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This separate-module consumer boots a custom distribution and drives the
// public HTTP host; a compile-only SPI test would miss host dispatch changes.
func TestExternalFormatCapabilitiesThroughHost(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path + "?" + r.URL.RawQuery
		mu.Lock()
		calls[key]++
		mu.Unlock()
		switch r.URL.Path {
		case "/file.txt":
			fmt.Fprint(w, "variant="+r.URL.RawQuery)
		case "/index.txt":
			if r.Header.Get("If-None-Match") == `"index-v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"index-v1"`)
			fmt.Fprint(w, "index-v1")
		case "/missing.txt":
			http.NotFound(w, r)
		case "/bad.txt":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	binary := filepath.Join(root, "suxen-custom")
	build := exec.Command("go", "build", "-o", binary, "./cmd/suxen-custom")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build custom server: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	base := "http://" + address
	const token = "example-module-bootstrap-token-12345"
	client := &http.Client{Timeout: 3 * time.Second}
	start := func(ttl string) func() {
		t.Helper()
		logFile, err := os.CreateTemp(root, "server-*.log")
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "serve")
		command.Env = append(os.Environ(),
			"SUXEN_DATA="+filepath.Join(root, "data"),
			"SUXEN_LISTEN="+address,
			"SUXEN_BOOTSTRAP_PASSWORD=example-password-12345",
			"SUXEN_BOOTSTRAP_TOKEN="+token,
			"SUXEN_OUTBOUND_ALLOWED_CIDRS=127.0.0.0/8",
			"SUXEN_PROXY_MANIFEST_TTL="+ttl,
		)
		command.Stdout, command.Stderr = logFile, logFile
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		var stopOnce sync.Once
		stop := func() {
			stopOnce.Do(func() {
				_ = command.Process.Signal(os.Interrupt)
				_ = command.Wait()
				logFile.Close()
			})
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get(base + "/readyz")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return stop
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		stop()
		log, _ := os.ReadFile(logFile.Name())
		t.Fatalf("server did not become ready: %s", log)
		return nil
	}
	request := func(method, path string, body []byte) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		content, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(content)
	}
	create := func(name, kind string, members []string) {
		t.Helper()
		payload := map[string]any{"name": name, "format": "exampletext", "type": kind}
		if kind == "proxy" {
			payload["upstream"] = upstream.URL
		} else if kind == "group" {
			payload["members"] = members
		} else {
			payload["allowOverwrite"] = false
		}
		body, _ := json.Marshal(payload)
		if status, result := request(http.MethodPost, "/api/v1/repositories", body); status != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, status, result)
		}
	}
	assertRead := func(path string, wantStatus int, wantBody string) {
		t.Helper()
		status, body := request(http.MethodGet, path, nil)
		if status != wantStatus || (wantBody != "" && body != wantBody) {
			t.Fatalf("GET %s = %d %q, want %d %q", path, status, body, wantStatus, wantBody)
		}
	}
	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[path]
	}

	stop := start("1h")
	create("external", "proxy", nil)
	create("external-group", "group", []string{"external"})
	for _, path := range []string{"/repository/external/file.txt?variant=one", "/repository/external-group/file.txt?variant=one"} {
		assertRead(path, http.StatusOK, "variant=variant=one")
	}
	assertRead("/repository/external/file.txt?variant=two", http.StatusOK, "variant=variant=two")
	assertRead("/repository/external/file.txt?variant=one", http.StatusOK, "variant=variant=one")
	if got := count("/file.txt?variant=one"); got != 1 {
		t.Fatalf("positive cache made %d upstream calls", got)
	}
	for range 2 {
		assertRead("/repository/external/missing.txt", http.StatusNotFound, "")
	}
	assertRead("/repository/external-group/missing.txt", http.StatusNotFound, "")
	if got := count("/missing.txt?"); got != 1 {
		t.Fatalf("negative cache made %d upstream calls", got)
	}
	assertRead("/repository/external/file.txt?deny=1", http.StatusBadRequest, "")
	if got := count("/file.txt?deny=1"); got != 0 {
		t.Fatalf("rejected request made %d upstream calls", got)
	}
	assertRead("/repository/external/bad.txt", http.StatusBadGateway, "")
	assertRead("/repository/external-group/bad.txt", http.StatusBadGateway, "")
	assertRead("/repository/external/index.txt", http.StatusOK, "index-v1")
	assertRead("/repository/external/index.txt", http.StatusOK, "index-v1")
	if got := count("/index.txt?"); got != 1 {
		t.Fatalf("fresh mutable cache made %d upstream calls", got)
	}
	create("external-hosted", "hosted", nil)
	for _, version := range []string{"1.0", "2.0"} {
		path := "/repository/external-hosted/_example/publish/widget/" + version
		if status, body := request(http.MethodPost, path, []byte("release-"+version)); status != http.StatusCreated {
			t.Fatalf("publish %s = %d %s", version, status, body)
		}
	}
	if status, body := request(http.MethodPost, "/repository/external-hosted/_example/publish/widget/2.0", []byte("changed")); status != http.StatusConflict {
		t.Fatalf("conflicting atomic publication = %d %s", status, body)
	}
	assertRead("/repository/external-hosted/pkg/widget/2.0/artifact.txt", http.StatusOK, "release-2.0")
	// The host owns replacement policy even for an external wire format.
	if status, body := request(http.MethodPut, "/api/v1/repositories/external-hosted", []byte(`{"format":"exampletext","type":"hosted","allowOverwrite":true}`)); status != http.StatusOK {
		t.Fatalf("enable replacement = %d %s", status, body)
	}
	if status, body := request(http.MethodPost, "/repository/external-hosted/_example/publish/widget/2.0", []byte("changed")); status != http.StatusCreated {
		t.Fatalf("allowed atomic replacement = %d %s", status, body)
	}
	assertRead("/repository/external-hosted/pkg/widget/2.0/artifact.txt", http.StatusOK, "changed")
	policy, _ := json.Marshal(map[string]any{
		"name": "external-keep", "repositories": []string{"external-hosted"},
		"criteria": []map[string]string{{"path": "sys.path", "op": "matches", "value": `/artifact\.txt$`}},
		"keepLast": 1, "action": "delete", "enabled": false,
	})
	if status, body := request(http.MethodPost, "/api/v1/cleanup-policies", policy); status != http.StatusCreated {
		t.Fatalf("create cleanup policy = %d %s", status, body)
	}
	if status, body := request(http.MethodPost, "/api/v1/cleanup-policies/external-keep/run?dryRun=false", nil); status != http.StatusOK || !strings.Contains(body, `"status":"succeeded"`) {
		t.Fatalf("run cleanup = %d %s", status, body)
	}
	status, body := request(http.MethodGet, "/api/v1/repositories/external-hosted/assets?limit=100", nil)
	if status != http.StatusOK {
		t.Fatalf("list hosted assets = %d %s", status, body)
	}
	var page struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, asset := range page.Items {
		paths[asset.Path] = true
	}
	if paths["pkg/widget/1.0/artifact.txt"] || paths["pkg/widget/1.0/meta.json"] ||
		!paths["pkg/widget/2.0/artifact.txt"] || !paths["pkg/widget/2.0/meta.json"] {
		t.Fatalf("external retention left wrong artifact/companion set: %v", paths)
	}
	stop()
	start("0")
	assertRead("/repository/external/index.txt", http.StatusOK, "index-v1")
	if got := count("/index.txt?"); got != 2 {
		t.Fatalf("revalidation made %d upstream calls", got)
	}
}
