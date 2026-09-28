package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/spi/blob"
	"github.com/suxen-project/suxen/spi/blob/blobtest"
)

func TestGCSConformance(t *testing.T) {
	blobtest.RunStoreSuite(t, func(t *testing.T) blob.Store {
		emulator := newFakeGCS(t, "test-bucket")
		store, err := open("gcs://test-bucket/some/prefix?endpoint=" + url.QueryEscape(emulator.URL))
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestWalkSkipsNoncanonicalDigestShards(t *testing.T) {
	emulator := newFakeGCS(t, "test-bucket")
	opened, err := open("gcs://test-bucket/some/prefix?endpoint=" + url.QueryEscape(emulator.URL))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*Store)
	ctx := context.Background()
	digestFor := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	canonical := digestFor("canonical")
	if _, err := store.Put(ctx, canonical, strings.NewReader("canonical")); err != nil {
		t.Fatal(err)
	}
	stray := digestFor("misplaced")
	strayKey := "some/prefix/sha256/wrong-shard/" + strings.TrimPrefix(stray, "sha256:")
	if _, err := store.uploadObject(ctx, strayKey, strings.NewReader("misplaced"), int64(len("misplaced")), false); err != nil {
		t.Fatal(err)
	}
	var walked []string
	if err := store.Walk(ctx, func(info blob.Info) error {
		walked = append(walked, info.Digest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || walked[0] != canonical {
		t.Fatalf("Walk yielded %v, want only canonical blob %s", walked, canonical)
	}
}

func TestDriverRegistration(t *testing.T) {
	driver, found := blob.Lookup("gcs")
	if !found {
		t.Fatal("gcs driver is not registered")
	}
	if !driver.SharedStorage {
		t.Fatal("gcs driver must report shared storage")
	}
	if driver.URLScheme != "gcs" {
		t.Fatalf("gcs driver URL scheme = %q", driver.URLScheme)
	}
	if _, found := blob.ForURL("gcs://bucket/prefix"); !found {
		t.Fatal("gcs:// URL does not resolve to the gcs driver")
	}
}

func TestParseConfiguration(t *testing.T) {
	valid := map[string]configuration{
		"gcs://bucket": {
			bucket: "bucket", endpoint: defaultEndpoint,
		},
		"gcs://bucket/deep/prefix": {
			bucket: "bucket", prefix: "deep/prefix", endpoint: defaultEndpoint,
		},
		"gcs://bucket/prefix?credentials=/etc/key.json": {
			bucket: "bucket", prefix: "prefix",
			endpoint: defaultEndpoint, credentials: "/etc/key.json",
		},
		"gcs://bucket?endpoint=http://emulator:4443": {
			bucket: "bucket", endpoint: "http://emulator:4443",
			credentials: "anonymous",
		},
	}
	for rawURL, want := range valid {
		parsed, err := parseConfiguration(rawURL)
		if err != nil {
			t.Fatalf("parseConfiguration(%q): %v", rawURL, err)
		}
		if parsed != want {
			t.Fatalf("parseConfiguration(%q) = %+v, want %+v", rawURL, parsed, want)
		}
	}

	invalid := []string{
		"s3://bucket",
		"gcs://",
		"gcs://bucket/../escape",
		"gcs://user:secret@bucket",
		"gcs://bucket?endpoint=ftp://host",
		"gcs://bucket?unknown=value",
	}
	for _, rawURL := range invalid {
		if _, err := parseConfiguration(rawURL); err == nil {
			t.Fatalf("parseConfiguration(%q) accepted an invalid configuration", rawURL)
		}
	}
}

func TestCanonicalConfigurationExcludesCredentials(t *testing.T) {
	first, err := canonicalConfiguration("gcs://bucket/prefix?credentials=/etc/a.json")
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicalConfiguration("gcs://bucket/prefix/?credentials=/etc/b.json")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("canonical configurations differ: %q vs %q", first, second)
	}
	distinct, err := canonicalConfiguration("gcs://bucket/other")
	if err != nil {
		t.Fatal(err)
	}
	if distinct == first {
		t.Fatal("different prefixes must canonicalize differently")
	}
}

// fakeGCS implements the JSON API subset the driver uses: media upload with
// ifGenerationMatch, metadata and media reads, prefix listing with paging,
// delete, and compose.
type fakeGCS struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]fakeObject
	nextGen int64
}

type fakeObject struct {
	data       []byte
	generation int64
	updated    time.Time
}

func newFakeGCS(t *testing.T, bucket string) *httptest.Server {
	fake := &fakeGCS{bucket: bucket, objects: make(map[string]fakeObject), nextGen: 1}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server
}

func (fake *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uploadPrefix := "/upload/storage/v1/b/" + fake.bucket + "/o"
	objectPrefix := "/storage/v1/b/" + fake.bucket + "/o"
	escaped := r.URL.EscapedPath()
	switch {
	case r.Method == http.MethodPost && escaped == uploadPrefix:
		fake.handleUpload(w, r)
	case escaped == objectPrefix && r.Method == http.MethodGet:
		fake.handleList(w, r)
	case strings.HasPrefix(escaped, objectPrefix+"/"):
		name, err := url.PathUnescape(strings.TrimPrefix(escaped, objectPrefix+"/"))
		if err != nil {
			http.Error(w, "bad object name", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(name, "/compose") {
			fake.handleCompose(w, r, strings.TrimSuffix(name, "/compose"))
			return
		}
		fake.handleObject(w, r, name)
	default:
		http.Error(w, "unhandled route "+r.Method+" "+escaped, http.StatusNotImplemented)
	}
}

func (fake *fakeGCS) handleUpload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("uploadType") != "media" {
		http.Error(w, "only media uploads are supported", http.StatusBadRequest)
		return
	}
	name := query.Get("name")
	if name == "" {
		http.Error(w, "missing object name", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if match := query.Get("ifGenerationMatch"); match != "" {
		generation := int64(0)
		if existing, found := fake.objects[name]; found {
			generation = existing.generation
		}
		if strconv.FormatInt(generation, 10) != match {
			http.Error(w, "generation mismatch", http.StatusPreconditionFailed)
			return
		}
	}
	object := fakeObject{data: data, generation: fake.nextGen, updated: time.Now()}
	fake.nextGen++
	fake.objects[name] = object
	fake.writeMetadata(w, name, object)
}

func (fake *fakeGCS) handleObject(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		fake.mu.Lock()
		object, found := fake.objects[name]
		fake.mu.Unlock()
		if !found {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(object.data)))
			_, _ = w.Write(object.data)
			return
		}
		fake.writeMetadata(w, name, object)
	case http.MethodDelete:
		fake.mu.Lock()
		_, found := fake.objects[name]
		delete(fake.objects, name)
		fake.mu.Unlock()
		if !found {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (fake *fakeGCS) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	pageToken := r.URL.Query().Get("pageToken")

	fake.mu.Lock()
	names := make([]string, 0, len(fake.objects))
	for name := range fake.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	// Single-item pages exercise the driver's pagination loop.
	items := make([]map[string]any, 0, 1)
	nextToken := ""
	for _, name := range names {
		if name <= pageToken {
			continue
		}
		object := fake.objects[name]
		items = append(items, map[string]any{
			"name":    name,
			"size":    strconv.Itoa(len(object.data)),
			"updated": object.updated.UTC().Format(time.RFC3339Nano),
		})
		if len(items) == 1 {
			nextToken = name
			break
		}
	}
	fake.mu.Unlock()

	remaining := false
	fake.mu.Lock()
	for _, name := range names {
		if name > nextToken {
			remaining = true
			break
		}
	}
	fake.mu.Unlock()
	response := map[string]any{"items": items}
	if nextToken != "" && remaining {
		response["nextPageToken"] = nextToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (fake *fakeGCS) handleCompose(w http.ResponseWriter, r *http.Request, destination string) {
	var request struct {
		SourceObjects []struct {
			Name string `json:"name"`
		} `json:"sourceObjects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if match := r.URL.Query().Get("ifGenerationMatch"); match != "" {
		generation := int64(0)
		if existing, found := fake.objects[destination]; found {
			generation = existing.generation
		}
		if strconv.FormatInt(generation, 10) != match {
			http.Error(w, "generation mismatch", http.StatusPreconditionFailed)
			return
		}
	}
	composed := make([]byte, 0)
	for _, source := range request.SourceObjects {
		object, found := fake.objects[source.Name]
		if !found {
			http.Error(w, "source object not found: "+source.Name, http.StatusNotFound)
			return
		}
		composed = append(composed, object.data...)
	}
	object := fakeObject{data: composed, generation: fake.nextGen, updated: time.Now()}
	fake.nextGen++
	fake.objects[destination] = object
	fake.writeMetadata(w, destination, object)
}

func (fake *fakeGCS) writeMetadata(w http.ResponseWriter, name string, object fakeObject) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":       name,
		"size":       strconv.Itoa(len(object.data)),
		"generation": strconv.FormatInt(object.generation, 10),
		"updated":    object.updated.UTC().Format(time.RFC3339Nano),
	})
}

func fakeObjectCount(t *testing.T, server *httptest.Server) int {
	t.Helper()
	response, err := http.Get(server.URL + "/storage/v1/b/test-bucket/o?prefix=")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return len(page.Items)
}

// TestAppendUploadCleansStagedParts covers the compose-based append: part
// objects must not leak after appends.
func TestAppendUploadCleansStagedParts(t *testing.T) {
	server := newFakeGCS(t, "test-bucket")
	opened, err := open("gcs://test-bucket?endpoint=" + url.QueryEscape(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	uploads := opened.(blob.UploadStore)

	ctx := t.Context()
	if err := uploads.CreateUpload(ctx, "session/one"); err != nil {
		t.Fatal(err)
	}
	for chunk := range 3 {
		if _, err := uploads.AppendUpload(
			ctx,
			"session/one",
			strings.NewReader(fmt.Sprintf("chunk-%d ", chunk)),
			1<<20,
		); err != nil {
			t.Fatal(err)
		}
	}
	reader, size, err := uploads.OpenUpload(ctx, "session/one")
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if string(content) != "chunk-0 chunk-1 chunk-2 " {
		t.Fatalf("upload content = %q", content)
	}
	if size != int64(len(content)) {
		t.Fatalf("upload size = %d, want %d", size, len(content))
	}
	// Session object only; every staged part must have been deleted.
	if count := fakeObjectCount(t, server); count != 1 {
		t.Fatalf("object count after appends = %d, want 1 (parts leaked)", count)
	}
}

// flakyProxy fails each distinct request a fixed number of times with a
// transient status before letting it through to the fake backend.
type flakyProxy struct {
	backend  http.Handler
	failures int
	mu       sync.Mutex
	seen     map[string]int
	attempts atomic.Int64
}

func (proxy *flakyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxy.attempts.Add(1)
	key := r.Method + " " + r.URL.String()
	proxy.mu.Lock()
	failed := proxy.seen[key]
	if failed < proxy.failures {
		proxy.seen[key] = failed + 1
	}
	proxy.mu.Unlock()
	if failed < proxy.failures {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy.backend.ServeHTTP(w, r)
}

func TestTransientFailuresAreRetried(t *testing.T) {
	fake := &fakeGCS{bucket: "test-bucket", objects: make(map[string]fakeObject), nextGen: 1}
	proxy := &flakyProxy{backend: fake, failures: 2, seen: make(map[string]int)}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	opened, err := open("gcs://test-bucket?endpoint=" + url.QueryEscape(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("retried content")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := opened.Put(t.Context(), digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put through transient failures: %v", err)
	}
	reader, _, err := opened.Get(t.Context(), digest)
	if err != nil {
		t.Fatalf("Get through transient failures: %v", err)
	}
	read, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(read, content) {
		t.Fatalf("read = %q err = %v", read, err)
	}

	// Requests failing past the retry budget surface the transient error.
	exhausted := &flakyProxy{backend: fake, failures: 100, seen: make(map[string]int)}
	exhaustedServer := httptest.NewServer(exhausted)
	t.Cleanup(exhaustedServer.Close)
	opened, err = open("gcs://test-bucket?endpoint=" + url.QueryEscape(exhaustedServer.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Head(t.Context(), digest); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("exhausted retries error = %v", err)
	}
	if got := exhausted.attempts.Load(); got != requestAttempts {
		t.Fatalf("exhausted attempts = %d, want %d", got, requestAttempts)
	}
}
