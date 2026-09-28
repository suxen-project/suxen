package server

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestOCIUploadCleanupAfterQuotaReduction(t *testing.T) {
	for _, cleanup := range []string{"cancel", "stale reaper"} {
		t.Run(cleanup, func(t *testing.T) {
			h := newOCIConcurrencyServer(t, true)
			t.Cleanup(func() { _ = h.Close() })
			updateQuota := func(limit int) {
				t.Helper()
				body := `{"driver":"tracking","configurationRef":{"env":"SUXEN_TEST_SECONDARY_STORE"},"attributes":{"uploadSessions":{"maxStagedBytes":` + strconv.Itoa(limit) + `,"maxPrincipalStagedBytes":` + strconv.Itoa(limit) + `}}}`
				response := blobStoreRequest(t, h, http.MethodPut, "/api/v1/blob-stores/secondary", body)
				if response.Code != http.StatusOK {
					t.Fatalf("set staged-byte quota to %d: %d %s", limit, response.Code, response.Body.String())
				}
			}
			updateQuota(10)
			start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
			if start.Code != http.StatusAccepted {
				t.Fatalf("start upload: %d %s", start.Code, start.Body.String())
			}
			location := start.Header().Get("Location")
			appendResponse := ociConcurrencyRequest(h, http.MethodPatch, location, "12345678", "")
			if appendResponse.Code != http.StatusAccepted {
				t.Fatalf("append upload: %d %s", appendResponse.Code, appendResponse.Body.String())
			}
			updateQuota(4)
			switch cleanup {
			case "cancel":
				response := ociConcurrencyRequest(h, http.MethodDelete, location, "", "")
				if response.Code != http.StatusNoContent {
					t.Fatalf("cancel upload above current quota: %d %s", response.Code, response.Body.String())
				}
			case "stale reaper":
				report, err := h.oci.ReapStaleUploadSessions(context.Background(), false, time.Now().UTC().Add(7*time.Hour))
				if err != nil || report.Deleted != 1 {
					t.Fatalf("reap upload above current quota = %+v, %v; want one deletion", report, err)
				}
			}
			remaining, err := h.metadata.CountUploadSessions(context.Background(), "secondary")
			if err != nil || remaining != 0 {
				t.Fatalf("remaining source upload sessions = %d, %v; want zero", remaining, err)
			}
		})
	}
}
