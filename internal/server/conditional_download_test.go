package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStoredAssetConditionalDownloads(t *testing.T) {
	fixture := newServerFixture(t)
	const path = "/repository/raw/conditional.bin"
	first := fixture.request(t, http.MethodPut, path, []byte("AAAAAA"), true)
	assertStatus(t, first, http.StatusCreated)
	first.Body.Close()
	old := conditionalRead(t, fixture, http.MethodGet, path, nil)
	assertStatus(t, old, http.StatusOK)
	oldETag := old.Header.Get("ETag")
	old.Body.Close()
	if oldETag == "" {
		t.Fatal("download did not advertise an ETag")
	}

	replacement := fixture.request(t, http.MethodPut, path, []byte("BBBBBB"), true)
	assertStatus(t, replacement, http.StatusCreated)
	replacement.Body.Close()
	current := conditionalRead(t, fixture, http.MethodGet, path, nil)
	assertStatus(t, current, http.StatusOK)
	currentETag := current.Header.Get("ETag")
	modified := current.Header.Get("Last-Modified")
	current.Body.Close()
	if currentETag == "" || currentETag == oldETag || modified == "" {
		t.Fatalf("invalid replacement validators: old=%q current=%q modified=%q", oldETag, currentETag, modified)
	}

	for _, test := range []struct {
		name    string
		method  string
		headers map[string]string
		status  int
		body    string
		rangeAt string
	}{
		{"stale If-Range sends full replacement", http.MethodGet, map[string]string{"Range": "bytes=3-", "If-Range": oldETag}, http.StatusOK, "BBBBBB", ""},
		{"current If-Range sends suffix", http.MethodGet, map[string]string{"Range": "bytes=3-", "If-Range": currentETag}, http.StatusPartialContent, "BBB", "bytes 3-5/6"},
		{"weak If-Range sends full replacement", http.MethodGet, map[string]string{"Range": "bytes=3-", "If-Range": "W/" + currentETag}, http.StatusOK, "BBBBBB", ""},
		{"stale date If-Range sends full replacement", http.MethodGet, map[string]string{"Range": "bytes=3-", "If-Range": time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)}, http.StatusOK, "BBBBBB", ""},
		{"current date If-Range sends full representation", http.MethodGet, map[string]string{"Range": "bytes=3-", "If-Range": modified}, http.StatusOK, "BBBBBB", ""},
		{"GET weak If-None-Match", http.MethodGet, map[string]string{"If-None-Match": "W/" + currentETag}, http.StatusNotModified, "", ""},
		{"HEAD current If-None-Match", http.MethodHead, map[string]string{"If-None-Match": currentETag}, http.StatusNotModified, "", ""},
		{"GET stale If-Match", http.MethodGet, map[string]string{"If-Match": oldETag}, http.StatusPreconditionFailed, "", ""},
		{"HEAD stale If-Match", http.MethodHead, map[string]string{"If-Match": oldETag}, http.StatusPreconditionFailed, "", ""},
		{"GET If-Match precedence", http.MethodGet, map[string]string{"If-Match": oldETag, "If-None-Match": currentETag}, http.StatusPreconditionFailed, "", ""},
		{"HEAD ignores satisfiable range", http.MethodHead, map[string]string{"Range": "bytes=3-", "If-Range": currentETag}, http.StatusOK, "", ""},
		{"HEAD ignores unsatisfiable range", http.MethodHead, map[string]string{"Range": "bytes=30-", "If-Range": currentETag}, http.StatusOK, "", ""},
		{"HEAD ignores malformed range", http.MethodHead, map[string]string{"Range": "bytes=not-a-range", "If-Range": currentETag}, http.StatusOK, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := conditionalRead(t, fixture, test.method, path, test.headers)
			defer response.Body.Close()
			assertStatus(t, response, test.status)
			if response.Header.Get("Content-Range") != test.rangeAt {
				t.Fatalf("Content-Range=%q, want %q", response.Header.Get("Content-Range"), test.rangeAt)
			}
			if test.method == http.MethodHead && test.status == http.StatusOK && response.Header.Get("Content-Length") != "6" {
				t.Fatalf("HEAD Content-Length=%q, want full representation length 6", response.Header.Get("Content-Length"))
			}
			if test.body != "" {
				assertBody(t, response, []byte(test.body))
			} else if response.Body != nil {
				buffer := new(bytes.Buffer)
				_, _ = buffer.ReadFrom(response.Body)
				if test.status == http.StatusNotModified && buffer.Len() != 0 {
					t.Fatalf("304 had body %q", buffer.String())
				}
			}
		})
	}

	gate := fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/raw/download-gate",
		[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}`),
		"application/json", testToken)
	assertStatus(t, gate, http.StatusCreated)
	gate.Body.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := conditionalRead(t, fixture, method, path, map[string]string{"If-None-Match": currentETag})
		assertStatus(t, response, http.StatusForbidden)
		response.Body.Close()
	}
}

func TestRepeatedValidatorFieldsEqualCombinedFields(t *testing.T) {
	fixture := newServerFixture(t)
	const rawPath = "/repository/raw/repeated-validators.bin"
	response := fixture.request(t, http.MethodPut, rawPath, []byte("payload"), true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()

	const manifestPath = "/repository/oci/v2/acme/repeated/manifests/latest"
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	response = fixture.requestWithContentType(t, http.MethodPut, manifestPath, manifest,
		"application/vnd.oci.image.index.v1+json", true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()

	for _, path := range []string{rawPath, manifestPath} {
		t.Run(path, func(t *testing.T) {
			current := conditionalRead(t, fixture, http.MethodGet, path, nil)
			assertStatus(t, current, http.StatusOK)
			etag := current.Header.Get("ETag")
			current.Body.Close()
			if etag == "" {
				t.Fatal("missing ETag")
			}
			for _, test := range []struct {
				name   string
				method string
				field  string
				status int
			}{
				{"matching If-Match", http.MethodGet, "If-Match", http.StatusOK},
				{"matching If-None-Match", http.MethodGet, "If-None-Match", http.StatusNotModified},
				{"HEAD matching If-Match", http.MethodHead, "If-Match", http.StatusOK},
				{"HEAD matching If-None-Match", http.MethodHead, "If-None-Match", http.StatusNotModified},
			} {
				t.Run(test.name, func(t *testing.T) {
					for _, repeated := range []bool{false, true} {
						request := httptest.NewRequest(test.method, path, nil)
						request.Header.Set("Authorization", "Bearer "+testToken)
						if repeated {
							request.Header.Add(test.field, `"other"`)
							request.Header.Add(test.field, etag)
						} else {
							request.Header.Set(test.field, `"other", `+etag)
						}
						recorder := httptest.NewRecorder()
						fixture.Handler.ServeHTTP(recorder, request)
						if got := recorder.Code; got != test.status {
							t.Errorf("repeated=%v status=%d, want %d", repeated, got, test.status)
						}
					}
				})
			}
		})
	}
}

func conditionalRead(t *testing.T, fixture *serverFixture, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)
	return response.Result()
}
