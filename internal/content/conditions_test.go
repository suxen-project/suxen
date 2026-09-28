package content

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadPreconditionsAndPrecedence(t *testing.T) {
	modified := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	old := modified.Add(-time.Hour).Format(http.TimeFormat)
	current := modified.Format(http.TimeFormat)
	newer := modified.Add(time.Hour).Format(http.TimeFormat)
	tests := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"strong If-Match", map[string]string{"If-Match": `"old", "sha256:current"`}, 0},
		{"weak If-Match fails", map[string]string{"If-Match": `W/"sha256:current"`}, http.StatusPreconditionFailed},
		{"If-Match overrides old If-Unmodified-Since", map[string]string{"If-Match": `"sha256:current"`, "If-Unmodified-Since": old}, 0},
		{"old If-Unmodified-Since fails", map[string]string{"If-Unmodified-Since": old}, http.StatusPreconditionFailed},
		{"current If-Unmodified-Since passes", map[string]string{"If-Unmodified-Since": current}, 0},
		{"weak If-None-Match returns 304", map[string]string{"If-None-Match": `W/"sha256:current"`}, http.StatusNotModified},
		{"If-None-Match ignores newer If-Modified-Since", map[string]string{"If-None-Match": `"old"`, "If-Modified-Since": newer}, 0},
		{"current If-Modified-Since returns 304", map[string]string{"If-Modified-Since": current}, http.StatusNotModified},
		{"old If-Modified-Since passes", map[string]string{"If-Modified-Since": old}, 0},
		{"If-Match failure wins over If-None-Match", map[string]string{"If-Match": `"old"`, "If-None-Match": `"sha256:current"`}, http.StatusPreconditionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/asset", nil)
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()
			response.Header().Set("ETag", `"sha256:current"`)
			response.Header().Set("Content-Type", "application/octet-stream")
			response.Header().Set("Content-Length", "7")
			proceed := checkReadPreconditions(response, request, modified)
			if test.status == 0 {
				if !proceed {
					t.Fatalf("precondition stopped a valid read with status %d", response.Code)
				}
				return
			}
			if proceed || response.Code != test.status {
				t.Fatalf("proceed=%v status=%d, want false and %d", proceed, response.Code, test.status)
			}
			if test.status == http.StatusNotModified {
				if response.Header().Get("Content-Type") != "" || response.Header().Get("Content-Length") != "" {
					t.Fatalf("304 retained representation headers: %v", response.Header())
				}
				if response.Header().Get("ETag") == "" {
					t.Fatal("304 lost ETag")
				}
			}
		})
	}
}

func TestIfRangeRequiresCurrentStrongValidator(t *testing.T) {
	modified := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	etag := `"sha256:current"`
	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{"absent", "", true},
		{"current strong ETag", etag, true},
		{"stale ETag", `"sha256:old"`, false},
		{"weak ETag", `W/"sha256:current"`, false},
		{"current date is not known strong", modified.Format(http.TimeFormat), false},
		{"stale date", modified.Add(-time.Second).Format(http.TimeFormat), false},
		{"future date", modified.Add(time.Second).Format(http.TimeFormat), false},
		{"malformed date", "yesterday", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ifRangeMatches(test.value, etag); got != test.want {
				t.Fatalf("ifRangeMatches(%q)=%v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestIfRangeRejectsDateWhenTwoGenerationsShareOneSecond(t *testing.T) {
	// Last-Modified has only whole-second precision. These two distinct
	// generations would advertise the same date despite different strong ETags.
	firstModified := time.Date(2026, time.September, 24, 12, 0, 0, 100, time.UTC)
	secondModified := firstModified.Add(500 * time.Millisecond)
	if firstModified.Format(http.TimeFormat) != secondModified.Format(http.TimeFormat) {
		t.Fatal("test setup did not create a Last-Modified collision")
	}
	date := firstModified.Format(http.TimeFormat)
	for _, etag := range []string{`"sha256:first"`, `"sha256:second"`} {
		if ifRangeMatches(date, etag) {
			t.Fatalf("date If-Range matched %s despite ambiguous generation", etag)
		}
	}
}

func TestSynthesizedContentConditionalReads(t *testing.T) {
	runtime := &Runtime{}
	content := []byte("derived index")
	initial := httptest.NewRecorder()
	runtime.serveSynthesizedContent(initial,
		httptest.NewRequest(http.MethodGet, "/index", nil), content, "text/plain")
	if initial.Code != http.StatusOK || initial.Body.String() != string(content) {
		t.Fatalf("initial response status=%d body=%q", initial.Code, initial.Body.String())
	}
	etag := initial.Header().Get("ETag")
	if etag == "" {
		t.Fatal("synthesized response has no ETag")
	}
	for _, test := range []struct {
		name   string
		method string
		header string
		value  string
		status int
	}{
		{"GET cache hit", http.MethodGet, "If-None-Match", etag, http.StatusNotModified},
		{"HEAD cache hit", http.MethodHead, "If-None-Match", "W/" + etag, http.StatusNotModified},
		{"GET stale If-Match", http.MethodGet, "If-Match", `"old"`, http.StatusPreconditionFailed},
		{"HEAD stale If-Match", http.MethodHead, "If-Match", `"old"`, http.StatusPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/index", nil)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()
			runtime.serveSynthesizedContent(response, request, content, "text/plain")
			if response.Code != test.status || response.Body.Len() != 0 {
				t.Fatalf("status=%d body=%q, want %d and no body", response.Code, response.Body.String(), test.status)
			}
			if response.Header().Get("ETag") != etag {
				t.Fatalf("ETag=%q, want %q", response.Header().Get("ETag"), etag)
			}
		})
	}
}
