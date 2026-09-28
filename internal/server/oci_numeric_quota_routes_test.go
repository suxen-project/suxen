package server

import (
	"net/http"
	"testing"
)

func TestOCINumericQuotaSpellingsEnforcedAfterAPIUpdate(t *testing.T) {
	handler := newOCIConcurrencyServer(t, true)
	t.Cleanup(func() { _ = handler.Close() })
	update := blobStoreRequest(t, handler, http.MethodPut, "/api/v1/blob-stores/secondary",
		`{"driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"},"attributes":{"uploadSessions":{"maxPrincipalSessions":1e0,"maxStagedBytes":10.0}}}`)
	if update.Code != http.StatusOK {
		t.Fatalf("save numeric upload policy: %d %s", update.Code, update.Body.String())
	}

	const uploadPath = "/repository/images/v2/app/blobs/uploads/"
	first := ociConcurrencyRequest(handler, http.MethodPost, uploadPath, "", "")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first upload: %d %s", first.Code, first.Body.String())
	}
	location := first.Header().Get("Location")
	if location == "" {
		t.Fatal("first upload has no Location")
	}
	second := ociConcurrencyRequest(handler, http.MethodPost, uploadPath, "", "")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second upload under maxPrincipalSessions=1e0: %d %s", second.Code, second.Body.String())
	}

	tooLarge := ociConcurrencyRequest(handler, http.MethodPatch, location, "12345678901", "")
	if tooLarge.Code != http.StatusTooManyRequests {
		t.Fatalf("11-byte append under maxStagedBytes=10.0: %d %s", tooLarge.Code, tooLarge.Body.String())
	}
	withinLimit := ociConcurrencyRequest(handler, http.MethodPatch, location, "1234567890", "")
	if withinLimit.Code != http.StatusAccepted {
		t.Fatalf("10-byte append at configured limit: %d %s", withinLimit.Code, withinLimit.Body.String())
	}
}
