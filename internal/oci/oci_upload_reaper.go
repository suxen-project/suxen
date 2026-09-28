package oci

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// StaleUploadReport summarises one stale upload-session sweep.
type StaleUploadReport struct {
	// Stale counts sessions whose inactivity exceeded their blob store's staleAfter.
	Stale int
	// Deleted counts sessions removed together with their staged upload object.
	Deleted int
	// Skipped counts stale sessions left in place because a live operation was racing.
	Skipped int
}

// ReapStaleUploadSessions removes upload sessions that were started and never
// completed or cancelled. Every session row counts against its principal's session
// quota, so leaked rows would otherwise reject new uploads forever. Each stale
// session is leased exactly like a client cancellation so a racing client request
// sees a busy lease or a missing upload rather than a half-deleted session.
func (h *Handler) ReapStaleUploadSessions(
	ctx context.Context,
	dryRun bool,
	now time.Time,
) (StaleUploadReport, error) {
	var report StaleUploadReport
	resources, err := h.meta().BlobStores(ctx)
	if err != nil {
		return report, fmt.Errorf("list blob stores: %w", err)
	}
	for _, resource := range resources {
		staleBefore := now.Add(-uploadSessionStaleAfter(resource))
		sessions, err := h.uploads().StaleUploadSessions(ctx, resource.Name, staleBefore, now)
		if err != nil {
			return report, fmt.Errorf("list stale upload sessions in %q: %w", resource.Name, err)
		}
		report.Stale += len(sessions)
		if dryRun || len(sessions) == 0 {
			continue
		}
		for _, session := range sessions {
			deleted, err := h.reapUploadSession(
				ctx, resource.Name, session, staleBefore, now,
			)
			if err != nil {
				return report, fmt.Errorf("reap upload session %s: %w", session.ID, err)
			}
			if deleted {
				report.Deleted++
			} else {
				report.Skipped++
			}
		}
	}
	return report, nil
}

func (h *Handler) reapUploadSession(
	ctx context.Context,
	blobStoreName string,
	session store.UploadSession,
	staleBefore time.Time,
	now time.Time,
) (bool, error) {
	operationID := httpx.RandomSecret(12)
	reservation, err := h.uploads().ReserveUploadSessionCleanup(
		ctx,
		session.UploadSessionIdentity,
		operationID,
		now,
		now.Add(time.Minute),
	)
	if errors.Is(err, domain.ErrUploadSessionBusy) || errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	release := true
	defer func() {
		if release {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = h.uploads().ReleaseUploadSessionOperationUncertain(
				releaseCtx,
				session.ID,
				operationID,
			)
		}
	}()
	// Selection is only a snapshot. A client may have completed an append before
	// this cleanup acquired the operation lease, so decide staleness again from
	// the reserved row before deleting its staged object.
	if !reservation.Session.UpdatedAt.Before(staleBefore) {
		return false, nil
	}

	backing, err := h.Runtime.BlobStores.Store(ctx, blobStoreName)
	if err != nil {
		return false, err
	}
	// A backend without upload support cannot hold a staged object for this row.
	if uploadStore, ok := backing.(blob.UploadStore); ok {
		err := uploadStore.DeleteUpload(ctx, session.StorageKey)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return false, fmt.Errorf("delete staged upload object: %w", err)
		}
	}
	if err := h.uploads().DeleteUploadSession(ctx, session.ID, operationID); err != nil {
		return false, err
	}
	release = false
	return true, nil
}
