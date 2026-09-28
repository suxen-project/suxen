package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSlowOCIPatchRenewsItsOperationLease(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	// Use a short lease to exercise several renewals without a five-minute test.
	h.oci.SetUploadLeaseDurationForTest(90 * time.Millisecond)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	reader, writer := io.Pipe()
	defer writer.Close()
	request := httptest.NewRequest(http.MethodPatch, location, reader)
	request.Header.Set("Authorization", "Bearer "+testToken)
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		patched <- response
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		session, err := h.metadata.UploadSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if session.OperationID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PATCH never acquired its operation lease")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A cancellation uses a cleanup reservation, which could take over after
	// expiry even while an append's reserved bytes are still charged.
	time.Sleep(240 * time.Millisecond)
	competing := ociConcurrencyRequest(h, http.MethodDelete, location, "", "")
	if competing.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent cancellation after original expiry = %d %s, want 429", competing.Code, competing.Body.String())
	}
	if _, err := writer.Write([]byte("slow-patch")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-patched:
		if result.Code != http.StatusAccepted {
			t.Fatalf("slow PATCH = %d %s", result.Code, result.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow PATCH did not finish")
	}
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-9" {
		t.Fatalf("stored upload = %d Range=%q %s", status.Code, status.Header().Get("Range"), status.Body.String())
	}
}
