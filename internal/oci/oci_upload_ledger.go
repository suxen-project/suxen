package oci

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

const (
	defaultUploadSessionStaleAfter = 6 * time.Hour
	DefaultUploadPrincipalSessions = int64(4)
)

type ociUploadState struct {
	store      blob.UploadStore
	storageKey string
	identity   store.UploadSessionIdentity
	limits     store.UploadSessionLimits
}

func (h *Handler) newOCIUploadState(
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) (ociUploadState, error) {
	if !validUploadID(uploadID) {
		return ociUploadState{}, domain.ErrNotFound
	}
	blobStoreName := repository.BlobStore
	storageKey := ociUploadStorageKey(repository.Name, uploadID)
	if blobStoreName == "" {
		blobStoreName = "default"
	}
	session, sessionErr := h.uploads().UploadSession(r.Context(), uploadID)
	switch {
	case sessionErr == nil:
		if session.Repository != repository.Name || session.Image != imageName ||
			session.Principal != uploadSessionPrincipal(r) {
			return ociUploadState{}, domain.ErrNotFound
		}
		blobStoreName = session.BlobStore
		storageKey = session.StorageKey
	case errors.Is(sessionErr, domain.ErrNotFound):
		var err error
		blobStoreName, err = h.meta().WriteBlobStore(r.Context(), blobStoreName)
		if err != nil {
			return ociUploadState{}, err
		}
	default:
		return ociUploadState{}, sessionErr
	}
	resource, err := h.meta().BlobStore(r.Context(), blobStoreName)
	if err != nil {
		return ociUploadState{}, err
	}
	backing, err := h.Runtime.BlobStores.Store(r.Context(), blobStoreName)
	if err != nil {
		return ociUploadState{}, err
	}
	uploadStore, ok := backing.(blob.UploadStore)
	if !ok {
		return ociUploadState{}, blob.ErrUploadStoreUnsupported
	}
	return ociUploadState{
		store:      uploadStore,
		storageKey: storageKey,
		identity: store.UploadSessionIdentity{
			ID:         uploadID,
			Repository: repository.Name,
			Image:      imageName,
			BlobStore:  blobStoreName,
			Principal:  uploadSessionPrincipal(r),
		},
		limits: uploadSessionLimits(resource, h.Runtime.Config.MaxUploadBytes),
	}, nil
}

func uploadSessionPrincipal(r *http.Request) string {
	requestContext := httpx.RequestLogFrom(r.Context())
	if requestContext == nil || requestContext.Subject == "" {
		return "anonymous"
	}
	return requestContext.Subject
}

func uploadSessionLimits(
	resource domain.BlobStore,
	maxUploadBytes int64,
) store.UploadSessionLimits {
	limits := store.UploadSessionLimits{
		MaxStagedBytes:          multipliedLimit(maxUploadBytes, 1),
		MaxPrincipalStagedBytes: multipliedLimit(maxUploadBytes, 1),
		MaxPrincipalSessions:    DefaultUploadPrincipalSessions,
	}
	attributes, ok := resource.Attributes["uploadSessions"].(map[string]any)
	if !ok {
		return limits
	}
	if value, found := positiveInt64(attributes["maxStagedBytes"]); found {
		limits.MaxStagedBytes = value
	}
	if value, found := positiveInt64(attributes["maxPrincipalStagedBytes"]); found {
		limits.MaxPrincipalStagedBytes = value
	}
	if value, found := positiveInt64(attributes["maxPrincipalSessions"]); found {
		limits.MaxPrincipalSessions = value
	}
	return limits
}

func uploadSessionStaleAfter(resource domain.BlobStore) time.Duration {
	attributes, ok := resource.Attributes["uploadSessions"].(map[string]any)
	if !ok {
		return defaultUploadSessionStaleAfter
	}
	value, ok := attributes["staleAfter"].(string)
	if !ok {
		return defaultUploadSessionStaleAfter
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return defaultUploadSessionStaleAfter
	}
	return duration
}

func multipliedLimit(value int64, multiplier int64) int64 {
	if value > 0 && value <= int64(^uint64(0)>>1)/multiplier {
		return value * multiplier
	}
	return int64(^uint64(0) >> 1)
}

func positiveInt64(value any) (int64, bool) {
	return domain.PositiveInt64(value)
}

func (h *Handler) reserveOCIUpload(
	r *http.Request,
	state ociUploadState,
	maxGrowth int64,
	exactGrowth bool,
) (store.UploadSessionReservation, string, error) {
	operationID := httpx.RandomSecret(12)
	duration := h.uploadOperationLeaseDuration()
	now := time.Now().UTC()
	reservation, err := h.uploads().ReserveUploadSession(
		r.Context(),
		state.identity,
		operationID,
		now,
		now.Add(duration),
		maxGrowth,
		exactGrowth,
		h.Runtime.Config.MaxUploadBytes,
		state.limits,
	)
	return reservation, operationID, err
}

func (h *Handler) uploadOperationLeaseDuration() time.Duration {
	if h.uploadLeaseDurationForTest > 0 {
		return h.uploadLeaseDurationForTest
	}
	if h.Runtime.Config.WriteTimeout > 0 {
		return h.Runtime.Config.WriteTimeout + time.Minute
	}
	return 5 * time.Minute
}

// startUploadOperationHeartbeat keeps a live append or completion reservation
// exclusive. Stopping the heartbeat or canceling the request leaves the finite
// lease to expire if this process crashes mid-operation.
func (h *Handler) startUploadOperationHeartbeat(
	parent context.Context,
	id string,
	operationID string,
) (context.Context, func() error) {
	duration := h.uploadOperationLeaseDuration()
	interval := duration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	var renewalErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deadline := duration / 2
				if deadline > 5*time.Second {
					deadline = 5 * time.Second
				}
				renewCtx, renewCancel := context.WithTimeout(ctx, deadline)
				now := time.Now().UTC()
				err := h.uploads().RenewUploadSessionOperation(
					renewCtx, id, operationID, now, now.Add(duration),
				)
				renewCancel()
				if err != nil {
					renewalErr = err
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	stop := func() error {
		once.Do(func() {
			cancel()
			<-done
		})
		return renewalErr
	}
	return ctx, stop
}

// reconcileOCIUploadSize repairs an upload whose physical append completed but
// whose ledger commit was interrupted. The second physical read occurs while
// holding the session operation, so an active append cannot be mistaken for an
// abandoned one. Reconciliation accounts for already-staged bytes regardless
// of current quotas; the subsequent growth/finalization reservation applies
// those limits before accepting more work.
func (h *Handler) reconcileOCIUploadSize(
	ctx context.Context,
	state ociUploadState,
) (int64, error) {
	session, err := h.uploads().UploadSession(ctx, state.identity.ID)
	if err != nil {
		return 0, err
	}
	if session.UploadSessionIdentity != state.identity {
		return 0, domain.ErrNotFound
	}
	reader, size, err := state.store.OpenUpload(ctx, state.storageKey)
	if err != nil {
		return 0, err
	}
	if err := reader.Close(); err != nil {
		return 0, err
	}
	if size == session.Size && session.ReservedBytes == 0 {
		return size, nil
	}

	operationID := httpx.RandomSecret(12)
	duration := h.Runtime.Config.WriteTimeout
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	now := time.Now().UTC()
	if _, err := h.uploads().ReserveUploadSessionCleanup(
		ctx, state.identity, operationID, now, now.Add(duration+time.Minute),
	); err != nil {
		return 0, err
	}
	released := false
	defer func() {
		if !released {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = h.uploads().ReleaseUploadSessionOperationUncertain(
				releaseCtx, state.identity.ID, operationID,
			)
		}
	}()
	reader, size, err = state.store.OpenUpload(ctx, state.storageKey)
	if err != nil {
		return 0, err
	}
	if err := reader.Close(); err != nil {
		return 0, err
	}
	if err := h.uploads().ReconcileUploadSessionSize(
		ctx, state.identity, operationID, size, time.Now().UTC(),
	); err != nil {
		return 0, err
	}
	released = true
	return size, nil
}

func writeOCIUploadLedgerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
	case errors.Is(err, domain.ErrUploadSessionSizeExceeded):
		httpx.WriteOCIError(
			w,
			http.StatusRequestEntityTooLarge,
			"BLOB_UPLOAD_INVALID",
			"upload exceeds the per-session size limit",
		)
	case errors.Is(err, domain.ErrUploadSessionQuotaExceeded),
		errors.Is(err, domain.ErrUploadSessionBusy):
		w.Header().Set("Retry-After", "1")
		httpx.WriteOCIError(
			w,
			http.StatusTooManyRequests,
			"TOOMANYREQUESTS",
			err.Error(),
		)
	default:
		httpx.WriteOCIInternalError(w, err)
	}
}
