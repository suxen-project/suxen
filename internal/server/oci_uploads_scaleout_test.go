package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
)

func TestOCIUploadSessionCanMoveBetweenReplicas(t *testing.T) {
	t.Parallel()
	firstReplica := newServerFixture(t)
	secondReplica := New(
		config.Config{
			DataDir:        t.TempDir(),
			MaxUploadBytes: 16 << 20,
		},
		firstReplica.Metadata,
		firstReplica.Handler.blobs,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	start := serveOCIUploadRequest(
		t,
		firstReplica.Handler,
		http.MethodPost,
		"/v2/team/scaleout/blobs/uploads/",
		nil,
	)
	assertStatus(t, start, http.StatusAccepted)
	uploadLocation := start.Header.Get("Location")
	start.Body.Close()
	if uploadLocation == "" {
		t.Fatal("upload response did not include a Location header")
	}

	firstChunk := serveOCIUploadRequest(
		t,
		secondReplica,
		http.MethodPatch,
		uploadLocation,
		[]byte("first-"),
	)
	assertStatus(t, firstChunk, http.StatusAccepted)
	firstChunk.Body.Close()

	status := serveOCIUploadRequest(
		t,
		firstReplica.Handler,
		http.MethodGet,
		uploadLocation,
		nil,
	)
	assertStatus(t, status, http.StatusNoContent)
	if uploadRange := status.Header.Get("Range"); uploadRange != "0-5" {
		t.Fatalf("upload range = %q, want 0-5", uploadRange)
	}
	status.Body.Close()

	content := []byte("first-second")
	completion := serveOCIUploadRequest(
		t,
		secondReplica,
		http.MethodPut,
		uploadLocation+"?digest="+testDigest(content),
		[]byte("second"),
	)
	assertStatus(t, completion, http.StatusCreated)
	completion.Body.Close()

	pulled := serveOCIUploadRequest(
		t,
		firstReplica.Handler,
		http.MethodGet,
		"/v2/team/scaleout/blobs/"+testDigest(content),
		nil,
	)
	assertStatus(t, pulled, http.StatusOK)
	assertBody(t, pulled, content)

	deletedSession := serveOCIUploadRequest(
		t,
		firstReplica.Handler,
		http.MethodGet,
		uploadLocation,
		nil,
	)
	assertStatus(t, deletedSession, http.StatusNotFound)
	deletedSession.Body.Close()
}

func serveOCIUploadRequest(
	t *testing.T,
	handler *Server,
	method string,
	requestPath string,
	body []byte,
) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
