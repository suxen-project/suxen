package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/store"
)

type interruptedUploadLedger struct {
	store.Store
	interrupt func()
}

func (m *interruptedUploadLedger) CommitUploadSessionAppend(
	ctx context.Context, id, operationID string, size int64, updatedAt time.Time, retainOperation bool,
) error {
	m.interrupt()
	return m.Store.CommitUploadSessionAppend(ctx, id, operationID, size, updatedAt, retainOperation)
}

func TestOCIUploadRecoversInterruptedLedgerCommit(t *testing.T) {
	for _, test := range []struct {
		name      string
		cancel    bool
		getStatus bool
	}{
		{"database failure then status", false, true},
		{"database failure then append", false, false},
		{"canceled request then status", true, true},
		{"canceled request then append", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newOCIConcurrencyServer(t, false)
			start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
			if start.Code != http.StatusAccepted {
				t.Fatalf("start = %d %s", start.Code, start.Body.String())
			}
			location := start.Header().Get("Location")
			id := start.Header().Get("Docker-Upload-UUID")
			request := httptest.NewRequest(http.MethodPatch, location, bytes.NewBufferString("abc"))
			request.Header.Set("Authorization", "Bearer "+testToken)
			request.Header.Set("Content-Range", "0-2")
			if test.cancel {
				ctx, cancel := context.WithCancel(request.Context())
				request = request.WithContext(ctx)
				h.content.SetMetadata(&interruptedUploadLedger{Store: h.metadata, interrupt: cancel})
			} else {
				h.content.SetMetadata(&failedUploadLedger{Store: h.metadata})
			}
			first := httptest.NewRecorder()
			h.ServeHTTP(first, request)
			h.content.SetMetadata(h.metadata)
			if first.Code != http.StatusInternalServerError {
				t.Fatalf("interrupted PATCH = %d %s", first.Code, first.Body.String())
			}
			session, err := h.metadata.UploadSession(context.Background(), id)
			if err != nil || session.Size != 0 {
				t.Fatalf("uncommitted ledger size = %d, %v; want 0", session.Size, err)
			}
			if test.getStatus {
				status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
				if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-2" {
					t.Fatalf("status = %d Range=%q %s", status.Code, status.Header().Get("Range"), status.Body.String())
				}
			}
			resumed := ociConcurrencyRequest(h, http.MethodPatch, location, "def", "3-5")
			if resumed.Code != http.StatusAccepted || resumed.Header().Get("Range") != "0-5" {
				t.Fatalf("resume = %d Range=%q %s", resumed.Code, resumed.Header().Get("Range"), resumed.Body.String())
			}
			session, err = h.metadata.UploadSession(context.Background(), id)
			if err != nil || session.Size != 6 {
				t.Fatalf("recovered ledger size = %d, %v; want 6", session.Size, err)
			}
			digest := sha256.Sum256([]byte("abcdef"))
			completed := ociConcurrencyRequest(h, http.MethodPut, location+"?digest=sha256:"+hex.EncodeToString(digest[:]), "", "")
			if completed.Code != http.StatusCreated {
				t.Fatalf("complete = %d %s", completed.Code, completed.Body.String())
			}
		})
	}
}

type failedUploadLedger struct{ store.Store }

func (m *failedUploadLedger) CommitUploadSessionAppend(context.Context, string, string, int64, time.Time, bool) error {
	return errors.New("simulated ledger write failure after physical append")
}

func TestOCIUploadRecoveryAccountsForBytesAfterQuotaReduction(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	h.content.SetMetadata(&failedUploadLedger{Store: h.metadata})
	failed := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2")
	h.content.SetMetadata(h.metadata)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("failed append = %d %s", failed.Code, failed.Body.String())
	}

	ctx := context.Background()
	resource, err := h.metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	resource.Attributes = map[string]any{"uploadSessions": map[string]any{
		"maxStagedBytes": int64(2), "maxPrincipalStagedBytes": int64(2),
	}}
	if err := h.metadata.(*store.SQLStore).UpdateBlobStore(ctx, resource); err != nil {
		t.Fatal(err)
	}
	// Accounting for already-written bytes must work even though the new quota
	// is smaller. It must not admit more bytes or let finalization skip the quota.
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-2" {
		t.Fatalf("status = %d Range=%q %s", status.Code, status.Header().Get("Range"), status.Body.String())
	}
	session, err := h.metadata.UploadSession(ctx, id)
	if err != nil || session.Size != 3 {
		t.Fatalf("reconciled size = %d, %v; want 3", session.Size, err)
	}
	growth := ociConcurrencyRequest(h, http.MethodPatch, location, "def", "3-5")
	if growth.Code != http.StatusTooManyRequests {
		t.Fatalf("growth beyond lowered quota = %d %s", growth.Code, growth.Body.String())
	}
	digest := sha256.Sum256([]byte("abc"))
	finalize := ociConcurrencyRequest(h, http.MethodPut, location+"?digest=sha256:"+hex.EncodeToString(digest[:]), "", "")
	if finalize.Code != http.StatusTooManyRequests {
		t.Fatalf("finalize beyond lowered quota = %d %s", finalize.Code, finalize.Body.String())
	}
	cancel := ociConcurrencyRequest(h, http.MethodDelete, location, "", "")
	if cancel.Code != http.StatusNoContent {
		t.Fatalf("cleanup beyond lowered quota = %d %s", cancel.Code, cancel.Body.String())
	}
}

func TestOCIUploadRecoveryDoesNotStealActiveOperation(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	h.content.SetMetadata(&failedUploadLedger{Store: h.metadata})
	failed := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2")
	h.content.SetMetadata(h.metadata)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("failed append = %d %s", failed.Code, failed.Body.String())
	}
	ctx := context.Background()
	session, err := h.metadata.UploadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "other-active-operation"
	now := time.Now().UTC()
	if _, err := h.metadata.ReserveUploadSessionCleanup(
		ctx, session.UploadSessionIdentity, operationID, now, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if status.Code != http.StatusTooManyRequests {
		t.Fatalf("status during active operation = %d %s", status.Code, status.Body.String())
	}
	session, err = h.metadata.UploadSession(ctx, id)
	if err != nil || session.OperationID != operationID || session.Size != 0 {
		t.Fatalf("recovery disturbed active operation: %+v, %v", session, err)
	}
	if err := h.metadata.ReleaseUploadSessionOperationUncertain(ctx, id, operationID); err != nil {
		t.Fatal(err)
	}
	status = ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-2" {
		t.Fatalf("status after lease release = %d Range=%q %s", status.Code, status.Header().Get("Range"), status.Body.String())
	}
}

func TestOCIUploadInterruptedAppendRefreshesStaleSessionActivity(t *testing.T) {
	h := newOCIConcurrencyServer(t, false)
	start := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if start.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	ctx := context.Background()
	session, err := h.metadata.UploadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	limits := store.UploadSessionLimits{MaxStagedBytes: 16 << 20, MaxPrincipalStagedBytes: 16 << 20, MaxPrincipalSessions: 4}
	now := time.Now().UTC()
	if _, err := h.metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity, "age-fixture", now, now.Add(time.Minute), 0, true, 16<<20, limits); err != nil {
		t.Fatal(err)
	}
	if err := h.metadata.CommitUploadSessionAppend(ctx, id, "age-fixture", 0, now.Add(-7*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	h.content.SetMetadata(&failedUploadLedger{Store: h.metadata})
	failed := ociConcurrencyRequest(h, http.MethodPatch, location, "abc", "0-2")
	h.content.SetMetadata(h.metadata)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("interrupted append = %d %s", failed.Code, failed.Body.String())
	}
	if _, err := h.oci.ReapStaleUploadSessions(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	status := ociConcurrencyRequest(h, http.MethodGet, location, "", "")
	if status.Code != http.StatusNoContent || status.Header().Get("Range") != "0-2" {
		t.Fatalf("reaper removed freshly active interrupted upload: %d Range=%q %s", status.Code, status.Header().Get("Range"), status.Body.String())
	}
}
