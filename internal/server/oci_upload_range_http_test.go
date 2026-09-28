package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/store"
)

// A real HTTP connection is required here: httptest.NewRequest gives its body
// a known length, while a client with unknown length sends chunked framing.
func TestOCIUploadHTTPContentRangeMatchesReceivedBytes(t *testing.T) {
	tests := []struct {
		name, method, body, contentRange string
		chunked, prefix, reject          bool
		wantStatus                       int
	}{
		{"chunked PATCH exact after prefix", http.MethodPatch, "cde", "2-4", true, true, false, http.StatusAccepted},
		{"chunked PATCH short after prefix", http.MethodPatch, "cde", "2-6", true, true, true, http.StatusRequestedRangeNotSatisfiable},
		{"chunked PATCH long after prefix", http.MethodPatch, "cde", "2-3", true, true, true, http.StatusRequestedRangeNotSatisfiable},
		{"chunked PATCH overflowing span", http.MethodPatch, "cde", "0-9223372036854775807", true, false, true, http.StatusBadRequest},
		{"chunked PUT exact after prefix", http.MethodPut, "cde", "2-4", true, true, false, http.StatusCreated},
		{"chunked PUT short after prefix", http.MethodPut, "cde", "2-6", true, true, true, http.StatusRequestedRangeNotSatisfiable},
		{"chunked PUT long after prefix", http.MethodPut, "cde", "2-3", true, true, true, http.StatusRequestedRangeNotSatisfiable},
		{"known PATCH exact after prefix", http.MethodPatch, "cde", "2-4", false, true, false, http.StatusAccepted},
		{"known PATCH mismatch after prefix", http.MethodPatch, "cde", "2-6", false, true, true, http.StatusRequestedRangeNotSatisfiable},
		{"known PUT mismatch after prefix", http.MethodPut, "cde", "2-3", false, true, true, http.StatusRequestedRangeNotSatisfiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newOCIConcurrencyServer(t, false)
			srv := httptest.NewServer(h)
			defer srv.Close()
			client := srv.Client()
			start := uploadRangeHTTP(t, client, srv.URL, http.MethodPost,
				"/repository/images/v2/app/blobs/uploads/", "", "", false)
			if start.StatusCode != http.StatusAccepted {
				t.Fatalf("start = %d", start.StatusCode)
			}
			location := start.Header.Get("Location")
			id := start.Header.Get("Docker-Upload-UUID")
			start.Body.Close()
			if location == "" || id == "" {
				t.Fatal("upload start did not return location and ID")
			}
			prefix := ""
			if tc.prefix {
				first := uploadRangeHTTP(t, client, srv.URL, http.MethodPatch, location, "ab", "0-1", false)
				if first.StatusCode != http.StatusAccepted {
					t.Fatalf("prefix = %d", first.StatusCode)
				}
				first.Body.Close()
				prefix = "ab"
			}
			beforeSession, beforeBytes := uploadRangeSnapshot(t, h, id)
			path := location
			if tc.method == http.MethodPut {
				path += "?digest=" + testDigest([]byte(prefix+tc.body))
			}
			response := uploadRangeHTTP(t, client, srv.URL, tc.method, path, tc.body, tc.contentRange, tc.chunked)
			if response.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				t.Fatalf("%s range %s, chunked=%t: status %d, want %d: %s",
					tc.method, tc.contentRange, tc.chunked, response.StatusCode, tc.wantStatus, body)
			}
			response.Body.Close()
			if !tc.reject {
				if tc.method == http.MethodPatch {
					assertUploadRangeState(t, h, id, prefix+tc.body)
				} else {
					assertUploadRangeBlob(t, client, srv.URL, prefix+tc.body)
				}
				return
			}

			// A refused chunk must preserve both the physical upload and the
			// ledger, including capacity and operation reservations.
			afterSession, afterBytes := uploadRangeSnapshot(t, h, id)
			if afterSession.Size != beforeSession.Size ||
				afterSession.ReservedBytes != beforeSession.ReservedBytes ||
				afterSession.OperationID != beforeSession.OperationID ||
				afterBytes != beforeBytes {
				t.Fatalf("rejected append changed upload: before=(size=%d reserved=%d operation=%q bytes=%q), after=(size=%d reserved=%d operation=%q bytes=%q)",
					beforeSession.Size, beforeSession.ReservedBytes, beforeSession.OperationID, beforeBytes,
					afterSession.Size, afterSession.ReservedBytes, afterSession.OperationID, afterBytes)
			}
			status := uploadRangeHTTP(t, client, srv.URL, http.MethodGet, location, "", "", false)
			if status.StatusCode != http.StatusNoContent || status.Header.Get("Range") != uploadRangeHeader(len(prefix)) {
				t.Fatalf("status after refusal = %d Range %q", status.StatusCode, status.Header.Get("Range"))
			}
			status.Body.Close()

			// The same session must remain usable with a corrected range.
			validRange := "0-2"
			if tc.prefix {
				validRange = "2-4"
			}
			retry := uploadRangeHTTP(t, client, srv.URL, tc.method, path, tc.body, validRange, tc.chunked)
			wantRetry := http.StatusAccepted
			if tc.method == http.MethodPut {
				wantRetry = http.StatusCreated
			}
			if retry.StatusCode != wantRetry {
				body, _ := io.ReadAll(retry.Body)
				retry.Body.Close()
				t.Fatalf("valid retry = %d, want %d: %s", retry.StatusCode, wantRetry, body)
			}
			retry.Body.Close()
			if tc.method == http.MethodPatch {
				assertUploadRangeState(t, h, id, prefix+tc.body)
			} else {
				assertUploadRangeBlob(t, client, srv.URL, prefix+tc.body)
			}
		})
	}
}

func uploadRangeHTTP(t *testing.T, client *http.Client, baseURL, method, path, body, contentRange string, chunked bool) *http.Response {
	t.Helper()
	var reader io.Reader = strings.NewReader(body)
	if chunked {
		reader = io.NopCloser(reader)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	if contentRange != "" {
		request.Header.Set("Content-Range", contentRange)
	}
	if chunked {
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func uploadRangeSnapshot(t *testing.T, h *Server, id string) (store.UploadSession, string) {
	t.Helper()
	ctx := context.Background()
	session, err := h.metadata.UploadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	backing, err := h.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	reader, size, err := backing.(blob.UploadStore).OpenUpload(ctx, ociUploadStorageKeyForTest("images", id))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	contents, err := io.ReadAll(reader)
	if err != nil || int64(len(contents)) != size {
		t.Fatalf("physical upload size %d, read %d bytes: %v", size, len(contents), err)
	}
	return session, string(contents)
}

func assertUploadRangeState(t *testing.T, h *Server, id, want string) {
	t.Helper()
	session, physical := uploadRangeSnapshot(t, h, id)
	if session.Size != int64(len(want)) || session.ReservedBytes != 0 || session.OperationID != "" || physical != want {
		t.Fatalf("upload state = size %d, reserved %d, operation %q, physical %q; want %q",
			session.Size, session.ReservedBytes, session.OperationID, physical, want)
	}
}

func assertUploadRangeBlob(t *testing.T, client *http.Client, baseURL, want string) {
	t.Helper()
	response := uploadRangeHTTP(t, client, baseURL, http.MethodGet,
		"/repository/images/v2/app/blobs/"+testDigest([]byte(want)), "", "", false)
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(contents) != want {
		t.Fatalf("published blob = status %d, bytes %q, error %v; want %q",
			response.StatusCode, contents, err, want)
	}
}

func uploadRangeHeader(size int) string {
	if size == 0 {
		return "0-0"
	}
	return "0-" + strconv.Itoa(size-1)
}

func TestOCIUploadHTTPChunkedRangeReservesDeclaredLength(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	ctx := context.Background()
	resource, err := h.metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	resource.Attributes = map[string]any{"uploadSessions": map[string]any{
		"maxStagedBytes": int64(8), "maxPrincipalStagedBytes": int64(8),
	}}
	if err := h.metadata.(*store.SQLStore).UpdateBlobStore(ctx, resource); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := srv.Client()
	start := uploadRangeHTTP(t, client, srv.URL, http.MethodPost,
		"/repository/images/v2/app/blobs/uploads/", "", "", false)
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d", start.StatusCode)
	}
	location, id := start.Header.Get("Location"), start.Header.Get("Docker-Upload-UUID")
	start.Body.Close()
	first := uploadRangeHTTP(t, client, srv.URL, http.MethodPatch, location, "ab", "0-1", false)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("prefix = %d", first.StatusCode)
	}
	first.Body.Close()
	other := uploadRangeHTTP(t, client, srv.URL, http.MethodPost,
		"/repository/images/v2/app/blobs/uploads/", "", "", false)
	if other.StatusCode != http.StatusAccepted {
		t.Fatalf("second upload start = %d", other.StatusCode)
	}
	otherLocation, otherID := other.Header.Get("Location"), other.Header.Get("Docker-Upload-UUID")
	other.Body.Close()

	pipeReader, pipeWriter := io.Pipe()
	defer pipeWriter.CloseWithError(io.ErrClosedPipe)
	request, err := http.NewRequest(http.MethodPatch, srv.URL+location, pipeReader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Range", "2-4")
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := client.Do(request)
		done <- result{response, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		session, err := h.metadata.UploadSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if session.OperationID != "" {
			if session.Size != 2 || session.ReservedBytes != 3 {
				t.Fatalf("in-flight upload = size %d, reserved %d; want size 2, reserved 3",
					session.Size, session.ReservedBytes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chunked upload never acquired a reservation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The 8-byte store has 2 staged bytes, 3 reserved bytes, and room for
	// this independent 3-byte chunk while the first HTTP body is still open.
	second := uploadRangeHTTP(t, client, srv.URL, http.MethodPatch, otherLocation, "xyz", "0-2", false)
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second upload under tight quota = %d, want 202", second.StatusCode)
	}
	second.Body.Close()
	assertUploadRangeState(t, h, otherID, "xyz")
	if _, err := io.WriteString(pipeWriter, "cde"); err != nil {
		t.Fatal(err)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-done:
		if completed.err != nil {
			t.Fatal(completed.err)
		}
		defer completed.response.Body.Close()
		if completed.response.StatusCode != http.StatusAccepted {
			t.Fatalf("chunked upload = %d, want 202", completed.response.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chunked upload did not finish after its body closed")
	}
	assertUploadRangeState(t, h, id, "abcde")
}

func TestOCIUploadHTTPEmptyFinalPutRejectsNonemptyRange(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := srv.Client()
	start := uploadRangeHTTP(t, client, srv.URL, http.MethodPost,
		"/repository/images/v2/app/blobs/uploads/", "", "", false)
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d", start.StatusCode)
	}
	location, id := start.Header.Get("Location"), start.Header.Get("Docker-Upload-UUID")
	start.Body.Close()
	first := uploadRangeHTTP(t, client, srv.URL, http.MethodPatch, location, "ab", "0-1", false)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("prefix = %d", first.StatusCode)
	}
	first.Body.Close()
	path := location + "?digest=" + testDigest([]byte("ab"))
	bad := uploadRangeHTTP(t, client, srv.URL, http.MethodPut, path, "", "2-2", true)
	if bad.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("empty final PUT with nonempty range = %d, want 416", bad.StatusCode)
	}
	bad.Body.Close()
	assertUploadRangeState(t, h, id, "ab")
	good := uploadRangeHTTP(t, client, srv.URL, http.MethodPut, path, "", "", true)
	if good.StatusCode != http.StatusCreated {
		t.Fatalf("empty final PUT retry = %d, want 201", good.StatusCode)
	}
	good.Body.Close()
	assertUploadRangeBlob(t, client, srv.URL, "ab")
}
