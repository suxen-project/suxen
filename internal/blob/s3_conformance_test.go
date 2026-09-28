package blob_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	spiblob "github.com/suxen-project/suxen/spi/blob"
	"github.com/suxen-project/suxen/spi/blob/blobtest"
)

// TestS3Conformance runs the shared blob conformance suite against the S3 store
// backed by an in-process fake, mirroring how the GCS driver is exercised
// against a fake emulator. It needs no external service and runs in the default
// (untagged) build; TestS3StoreIntegration covers a real S3 endpoint under the
// suxen_integration tag.
func TestS3Conformance(t *testing.T) {
	blobtest.RunStoreSuite(t, func(t *testing.T) spiblob.Store {
		// The store loads credentials from the default chain and the SDK adds a
		// default request checksum that would frame the body as aws-chunked;
		// pin dummy static credentials and disable that so the fake sees a plain
		// PutObject body.
		t.Setenv("AWS_ACCESS_KEY_ID", "suxen-test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "suxen-test-secret")
		t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
		t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_required")

		server := newFakeS3(t, "conformance")
		store, err := blob.NewS3(
			"s3://conformance?region=us-east-1&pathStyle=true&endpoint=" +
				url.QueryEscape(server.URL),
		)
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestS3WalkSkipsNoncanonicalDigestShards(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "suxen-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "suxen-test-secret")
	canonical := strings.Repeat("a", 64)
	stray := strings.Repeat("b", 64)
	fake := &fakeS3{
		bucket: "walk-test",
		objects: map[string]fakeS3Object{
			"prefix/sha256/aa/aa/" + canonical:   {data: []byte("canonical"), modified: time.Now()},
			"prefix/sha256/wrong-shard/" + stray: {data: []byte("misplaced"), modified: time.Now()},
		},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store, err := blob.NewS3("s3://walk-test/prefix?endpoint=" + url.QueryEscape(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	var walked []string
	if err := store.Walk(context.Background(), func(info spiblob.Info) error {
		walked = append(walked, info.Digest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || walked[0] != "sha256:"+canonical {
		t.Fatalf("Walk yielded %v, want only canonical blob", walked)
	}
}

// fakeS3 is a minimal in-memory, path-style S3 endpoint: enough of the object
// API (PUT/GET/HEAD/DELETE plus ListObjectsV2 and If-None-Match) for the blob
// conformance suite. It is not a general S3 emulator.
type fakeS3 struct {
	bucket  string
	mu      sync.Mutex
	objects map[string]fakeS3Object
}

type fakeS3Object struct {
	data     []byte
	modified time.Time
}

func newFakeS3(t *testing.T, bucket string) *httptest.Server {
	t.Helper()
	fake := &fakeS3{bucket: bucket, objects: make(map[string]fakeS3Object)}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server
}

func (fake *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + fake.bucket
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		fake.writeError(w, http.StatusNotFound, "NoSuchBucket", "unknown bucket")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")

	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			fake.list(w, r)
			return
		}
		fake.get(w, key, true)
	case http.MethodHead:
		fake.get(w, key, false)
	case http.MethodPut:
		fake.put(w, r, key)
	case http.MethodDelete:
		fake.delete(w, key)
	default:
		fake.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (fake *fakeS3) put(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fake.writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if r.Header.Get("If-None-Match") == "*" {
		if _, exists := fake.objects[key]; exists {
			fake.writeError(w, http.StatusPreconditionFailed, "PreconditionFailed",
				"object already exists")
			return
		}
	}
	fake.objects[key] = fakeS3Object{data: body, modified: time.Now().UTC()}
	w.Header().Set("ETag", fakeS3ETag(body))
	w.WriteHeader(http.StatusOK)
}

func (fake *fakeS3) get(w http.ResponseWriter, key string, writeBody bool) {
	fake.mu.Lock()
	object, exists := fake.objects[key]
	fake.mu.Unlock()
	if !exists {
		// A HEAD carries no body, so the SDK maps a bare 404 to NotFound; GET
		// returns the NoSuchKey error document.
		if writeBody {
			fake.writeError(w, http.StatusNotFound, "NoSuchKey", "no such key")
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(object.data)))
	w.Header().Set("Last-Modified", object.modified.Format(http.TimeFormat))
	w.Header().Set("ETag", fakeS3ETag(object.data))
	w.WriteHeader(http.StatusOK)
	if writeBody {
		_, _ = w.Write(object.data)
	}
}

func (fake *fakeS3) delete(w http.ResponseWriter, key string) {
	fake.mu.Lock()
	delete(fake.objects, key)
	fake.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (fake *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	fake.mu.Lock()
	keys := make([]string, 0, len(fake.objects))
	for key := range fake.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := listBucketResult{Name: fake.bucket, Prefix: prefix, MaxKeys: 1000}
	for _, key := range keys {
		object := fake.objects[key]
		result.Contents = append(result.Contents, listContents{
			Key:          key,
			Size:         int64(len(object.data)),
			LastModified: object.modified.Format(time.RFC3339),
			ETag:         fakeS3ETag(object.data),
		})
	}
	fake.mu.Unlock()
	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(result)
}

func (fake *fakeS3) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: message})
}

func fakeS3ETag(data []byte) string {
	// A non-empty opaque validator; the store never checks its value.
	return fmt.Sprintf("%q", fmt.Sprintf("%x", len(data)))
}

type listBucketResult struct {
	XMLName  xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name     string         `xml:"Name"`
	Prefix   string         `xml:"Prefix"`
	KeyCount int            `xml:"KeyCount"`
	MaxKeys  int            `xml:"MaxKeys"`
	Contents []listContents `xml:"Contents"`
}

type listContents struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}
