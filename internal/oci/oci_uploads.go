package oci

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/store"
)

func (h *Handler) handleOCIUploadRoute(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) {
	if repository.Type != "hosted" {
		httpx.WriteResult(w, nil, domain.ErrReadOnly)
		return
	}
	if uploadID == "" {
		if r.Method != http.MethodPost {
			httpx.MethodNotAllowed(w, http.MethodPost)
			return
		}
		h.startOCIUpload(w, r, repository, imageName)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		h.appendOCIUpload(w, r, repository, imageName, uploadID)
	case http.MethodPut:
		h.completeOCIUpload(w, r, repository, imageName, uploadID)
	case http.MethodGet:
		h.getOCIUploadStatus(w, r, repository, imageName, uploadID)
	case http.MethodDelete:
		h.cancelOCIUpload(w, r, repository, imageName, uploadID)
	default:
		httpx.MethodNotAllowed(
			w,
			http.MethodPatch,
			http.MethodPut,
			http.MethodGet,
			http.MethodDelete,
		)
	}
}

func (h *Handler) startOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
) {
	if digest := r.URL.Query().Get("digest"); digest != "" &&
		r.URL.Query().Get("mount") == "" {
		h.completeMonolithicOCIUpload(w, r, repository, imageName, digest)
		return
	}
	if h.tryMountOCIBlob(w, r, repository, imageName) {
		return
	}

	uploadID := newUploadID()
	storeMoved := errors.New("upload store binding moved")
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		current, readErr := h.meta().Repository(r.Context(), repository.Name)
		if readErr != nil {
			err = readErr
			break
		}
		state, stateErr := h.newOCIUploadState(r, current, imageName, uploadID)
		if stateErr != nil {
			err = stateErr
			break
		}
		now := time.Now().UTC()
		err = h.Runtime.WithBlobStoreLease(r.Context(), state.identity.BlobStore, func(leaseCtx context.Context) error {
			// Migration holds this lease while checking for source sessions
			// and rebinding repositories. Recheck the binding because it may
			// have moved before we acquired the lease.
			locked, readErr := h.meta().Repository(leaseCtx, repository.Name)
			if readErr != nil {
				return readErr
			}
			writeStore, readErr := h.meta().WriteBlobStore(leaseCtx, locked.BlobStore)
			if readErr != nil {
				return readErr
			}
			if writeStore != state.identity.BlobStore {
				return storeMoved
			}
			if err := h.uploads().CreateUploadSession(leaseCtx, store.UploadSession{
				UploadSessionIdentity: state.identity,
				RepositoryID:          repository.ID,
				StorageKey:            state.storageKey,
				CreatedAt:             now,
				UpdatedAt:             now,
			}, state.limits); err != nil {
				return err
			}
			if err := state.store.CreateUpload(leaseCtx, state.storageKey); err != nil {
				// A backend may have created the object before returning an
				// ambiguous error or the request may have been canceled. Remove
				// physical bytes first, then the quota-counting row, with a bounded
				// independent context. If physical cleanup fails, retain the row
				// so the stale-session reaper can find and remove those bytes.
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(leaseCtx), 5*time.Second)
				defer cancel()
				deleteObjectErr := state.store.DeleteUpload(cleanupCtx, state.storageKey)
				if deleteObjectErr == nil || errors.Is(deleteObjectErr, domain.ErrNotFound) {
					if cleanupErr := h.uploads().DeleteUploadSession(cleanupCtx, uploadID, ""); cleanupErr != nil {
						h.requestLogger(r).Warn("delete failed OCI upload session", "error", cleanupErr)
					}
				} else {
					h.requestLogger(r).Warn("delete failed OCI upload object", "error", deleteObjectErr)
				}
				return err
			}
			return nil
		})
		if !errors.Is(err, storeMoved) {
			break
		}
	}
	if err != nil {
		if errors.Is(err, storeMoved) {
			w.Header().Set("Retry-After", "1")
			httpx.WriteOCIError(w, http.StatusServiceUnavailable, "TOOMANYREQUESTS", "upload store moved; retry")
			return
		}
		writeOCIUploadLedgerError(w, err)
		return
	}

	writeOCIUploadHeaders(w, r, repository.Name, imageName, uploadID, 0)
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) appendOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) {
	state, err := h.newOCIUploadState(r, repository, imageName, uploadID)
	if err != nil {
		writeOCIUploadStoreError(w, err)
		return
	}
	if _, err := h.reconcileOCIUploadSize(r.Context(), state); err != nil {
		writeOCIUploadLedgerError(w, err)
		return
	}
	if !h.preflightOCIUploadContentRange(w, r, state) {
		return
	}
	size, _, ok := h.appendReservedOCIUpload(w, r, state, false)
	if !ok {
		return
	}

	writeOCIUploadHeaders(w, r, repository.Name, imageName, uploadID, size)
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) completeOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) {
	state, err := h.newOCIUploadState(r, repository, imageName, uploadID)
	if err != nil {
		writeOCIUploadStoreError(w, err)
		return
	}
	if _, err := h.reconcileOCIUploadSize(r.Context(), state); err != nil {
		writeOCIUploadLedgerError(w, err)
		return
	}

	operationID := ""
	if r.ContentLength != 0 || len(r.Header.Values("Content-Range")) != 0 {
		if !h.preflightOCIUploadContentRange(w, r, state) {
			return
		}
		var ok bool
		_, operationID, ok = h.appendReservedOCIUpload(w, r, state, true)
		if !ok {
			return
		}
	} else {
		_, operationID, err = h.reserveOCIUpload(r, state, 0, true)
		if err != nil {
			writeOCIUploadLedgerError(w, err)
			return
		}
	}
	releaseOperation := true
	defer func() {
		if releaseOperation {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			_ = h.uploads().ReleaseUploadSessionOperationUncertain(
				releaseCtx,
				uploadID,
				operationID,
			)
		}
	}()
	completionCtx, stopHeartbeat := h.startUploadOperationHeartbeat(r.Context(), uploadID, operationID)
	defer stopHeartbeat()

	digest, err := blob.NormalizeDigest(r.URL.Query().Get("digest"))
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	reader, size, err := state.store.OpenUpload(completionCtx, state.storageKey)
	if errors.Is(err, domain.ErrNotFound) {
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
		return
	}
	if err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	defer reader.Close()
	staged, err := h.Runtime.StageUpload(w, reader)
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	defer staged.Remove()
	if staged.Digest != digest {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "uploaded content does not match requested digest")
		return
	}

	assetPath := ocimodel.BlobPath(imageName, digest)
	_, _, err = h.Runtime.CommitStagedAssets(completionCtx, repository.Name, []content.StagedUpload{staged}, []domain.Asset{{
		Repository:   repository.Name,
		RepositoryID: repository.ID,
		Path:         assetPath,
		Digest:       digest,
		Size:         size,
		ContentType:  "application/octet-stream",
		Kind:         "oci-blob",
	}})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := completionCtx.Err(); err != nil {
		httpx.WriteOCIInternalError(w, fmt.Errorf("OCI upload operation lease ended: %w", err))
		return
	}
	if err := h.uploads().DeleteUploadSession(
		context.WithoutCancel(r.Context()),
		uploadID,
		operationID,
	); err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	releaseOperation = false
	if err := state.store.DeleteUpload(r.Context(), state.storageKey); err != nil &&
		!errors.Is(err, domain.ErrNotFound) {
		h.requestLogger(r).Warn("delete completed OCI upload object", "error", err)
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", ociLocationForRequest(r, repository.Name, assetPath))
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) completeMonolithicOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	digestValue string,
) {
	digest, err := blob.NormalizeDigest(digestValue)
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	staged, err := h.Runtime.StageUpload(w, r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if content.IsUploadTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		httpx.WriteOCIError(w, status, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}
	defer staged.Remove()
	if staged.Digest != digest {
		message := fmt.Sprintf("expected %s, got %s", digest, staged.Digest)
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", message)
		return
	}

	assetPath := ocimodel.BlobPath(imageName, digest)
	_, _, err = h.Runtime.CommitStagedAssets(r.Context(), repository.Name, []content.StagedUpload{staged}, []domain.Asset{{
		Repository:   repository.Name,
		RepositoryID: repository.ID,
		Path:         assetPath,
		Digest:       digest,
		Size:         staged.Size,
		ContentType:  "application/octet-stream",
		Kind:         "oci-blob",
	}})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", ociLocationForRequest(r, repository.Name, assetPath))
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) appendReservedOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	state ociUploadState,
	retainOperation bool,
) (int64, string, bool) {
	var expected int64
	rangeStart, rangeEnd, hasRange, rangeErr := ociRequestContentRange(r)
	if rangeErr != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", rangeErr.Error())
		return 0, "", false
	}
	if hasRange {
		expected = rangeEnd - rangeStart + 1
		if r.ContentLength >= 0 && r.ContentLength != expected {
			httpx.WriteOCIError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "Content-Range does not match the request body")
			return 0, "", false
		}
	}
	maxGrowth := h.Runtime.Config.MaxUploadBytes
	exactGrowth := r.ContentLength >= 0
	if hasRange {
		maxGrowth = expected
		exactGrowth = true
	} else if exactGrowth {
		maxGrowth = r.ContentLength
		expected = r.ContentLength
	}
	reservation, operationID, err := h.reserveOCIUpload(
		r,
		state,
		maxGrowth,
		exactGrowth,
	)
	if err != nil {
		writeOCIUploadLedgerError(w, err)
		return 0, "", false
	}
	release := true
	uncertain := false
	defer func() {
		if release {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if uncertain {
				_ = h.uploads().ReleaseUploadSessionOperationUncertain(
					releaseCtx, state.identity.ID, operationID,
				)
			} else {
				_ = h.uploads().ReleaseUploadSessionOperation(
					releaseCtx, state.identity.ID, operationID,
				)
			}
		}
	}()
	// The reservation serializes all operations on this upload. Validate the
	// caller's offset against that authoritative snapshot so two concurrent
	// requests cannot both append after observing the same earlier size.
	if hasRange && rangeStart != reservation.Session.Size {
		writeOCIUploadHeaders(
			w,
			r,
			state.identity.Repository,
			state.identity.Image,
			state.identity.ID,
			reservation.Session.Size,
		)
		httpx.WriteOCIError(
			w,
			http.StatusRequestedRangeNotSatisfiable,
			"BLOB_UPLOAD_INVALID",
			"Content-Range does not match the current upload offset",
		)
		return 0, "", false
	}
	appendCtx, stopHeartbeat := h.startUploadOperationHeartbeat(r.Context(), state.identity.ID, operationID)
	defer stopHeartbeat()
	source := io.Reader(r.Body)
	if exactGrowth {
		source = &ociUploadLengthReader{source: source, remaining: expected}
	}
	size, err := state.store.AppendUpload(
		appendCtx,
		state.storageKey,
		source,
		reservation.MaxSize,
	)
	if errors.Is(err, errOCIUploadLengthMismatch) {
		status := http.StatusBadRequest
		if hasRange {
			status = http.StatusRequestedRangeNotSatisfiable
			writeOCIUploadHeaders(w, r, state.identity.Repository, state.identity.Image, state.identity.ID, reservation.Session.Size)
		}
		httpx.WriteOCIError(w, status, "BLOB_UPLOAD_INVALID", "upload body does not match the declared length")
		return 0, "", false
	}
	if errors.Is(err, domain.ErrNotFound) {
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
		return 0, "", false
	}
	if errors.Is(err, blob.ErrUploadTooLarge) {
		if reservation.MaxSize < h.Runtime.Config.MaxUploadBytes {
			writeOCIUploadLedgerError(w, domain.ErrUploadSessionQuotaExceeded)
		} else {
			writeOCIUploadLedgerError(w, domain.ErrUploadSessionSizeExceeded)
		}
		return 0, "", false
	}
	if err != nil {
		// A backend may have committed the append before reporting a transport
		// failure. Keep its capacity reserved until physical reconciliation.
		uncertain = true
		httpx.WriteOCIInternalError(w, err)
		return 0, "", false
	}
	uncertain = true
	if err := h.uploads().CommitUploadSessionAppend(
		appendCtx,
		state.identity.ID,
		operationID,
		size,
		time.Now().UTC(),
		retainOperation,
	); err != nil {
		httpx.WriteOCIInternalError(w, err)
		return 0, "", false
	}
	if heartbeatErr := stopHeartbeat(); heartbeatErr != nil && retainOperation {
		httpx.WriteOCIInternalError(w, fmt.Errorf("renew OCI upload operation lease: %w", heartbeatErr))
		return 0, "", false
	}
	release = false
	return size, operationID, true
}

func (h *Handler) getOCIUploadStatus(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) {
	state, err := h.newOCIUploadState(r, repository, imageName, uploadID)
	if err != nil {
		writeOCIUploadStoreError(w, err)
		return
	}
	session, err := h.uploads().UploadSession(r.Context(), uploadID)
	if err == nil && session.UploadSessionIdentity != state.identity {
		err = domain.ErrNotFound
	}
	if err != nil {
		writeOCIUploadLedgerError(w, err)
		return
	}
	reader, size, err := state.store.OpenUpload(r.Context(), state.storageKey)
	if errors.Is(err, domain.ErrNotFound) {
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
		return
	}
	if err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	_ = reader.Close()
	if size != session.Size || session.ReservedBytes != 0 {
		size, err = h.reconcileOCIUploadSize(r.Context(), state)
		if err != nil {
			writeOCIUploadLedgerError(w, err)
			return
		}
	}
	writeOCIUploadHeaders(w, r, repository.Name, imageName, uploadID, size)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) cancelOCIUpload(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	uploadID string,
) {
	state, err := h.newOCIUploadState(r, repository, imageName, uploadID)
	if err != nil {
		writeOCIUploadStoreError(w, err)
		return
	}
	session, err := h.uploads().UploadSession(r.Context(), uploadID)
	if err != nil {
		writeOCIUploadLedgerError(w, err)
		return
	}
	if session.UploadSessionIdentity != state.identity {
		writeOCIUploadLedgerError(w, domain.ErrNotFound)
		return
	}
	operationID := httpx.RandomSecret(12)
	now := time.Now().UTC()
	duration := h.Runtime.Config.WriteTimeout
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	_, err = h.uploads().ReserveUploadSessionCleanup(
		r.Context(), state.identity, operationID,
		now, now.Add(duration+time.Minute),
	)
	if err != nil {
		writeOCIUploadLedgerError(w, err)
		return
	}
	releaseOperation := true
	defer func() {
		if releaseOperation {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			_ = h.uploads().ReleaseUploadSessionOperationUncertain(
				releaseCtx,
				uploadID,
				operationID,
			)
		}
	}()
	if err := state.store.DeleteUpload(r.Context(), state.storageKey); errors.Is(err, domain.ErrNotFound) {
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
		return
	} else if err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	if err := h.uploads().DeleteUploadSession(r.Context(), uploadID, operationID); err != nil {
		httpx.WriteOCIInternalError(w, err)
		return
	}
	releaseOperation = false
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) tryMountOCIBlob(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
) bool {
	mount := r.URL.Query().Get("mount")
	from := r.URL.Query().Get("from")
	if mount == "" || from == "" {
		return false
	}

	digest, err := blob.NormalizeDigest(mount)
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return true
	}

	source, err := h.metaFor(repository).Asset(r.Context(), ocimodel.BlobPath(from, digest))
	if errors.Is(err, domain.ErrNotFound) {
		return false
	}
	if err != nil {
		httpx.WriteOCIInternalError(w, err)
		return true
	}

	denial, err := h.Runtime.DownloadAllowed(r.Context(), source)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"download_gate_error",
			"evaluate download gate failed",
			err,
		)
		return true
	}
	if denial != "" {
		httpx.WriteProblem(
			w,
			http.StatusForbidden,
			denial,
			"source blob is not downloadable",
		)
		return true
	}

	var asset domain.Asset
	storeMoved := errors.New("source blob moved during mount")
	for attempt := 0; attempt < 3; attempt++ {
		// Migration may finish between this read and lease acquisition. Retry
		// with the new physical store instead of publishing under a stale one.
		current, readErr := h.metaFor(repository).Asset(r.Context(), ocimodel.BlobPath(from, digest))
		if readErr != nil {
			err = readErr
			break
		}
		err = h.Runtime.WithBlobStoreLease(r.Context(), current.BlobStore, func(leaseCtx context.Context) error {
			locked, readErr := h.metaFor(repository).Asset(leaseCtx, ocimodel.BlobPath(from, digest))
			if readErr != nil {
				return readErr
			}
			if locked.BlobStore != current.BlobStore {
				return storeMoved
			}
			var putErr error
			asset, putErr = h.meta().PutAsset(leaseCtx, domain.Asset{
				Repository: repository.Name, RepositoryID: repository.ID, Path: ocimodel.BlobPath(imageName, digest),
				Digest: locked.Digest, Size: locked.Size, ContentType: locked.ContentType,
				BlobStore: locked.BlobStore, Kind: "oci-blob",
			})
			return putErr
		})
		if !errors.Is(err, storeMoved) {
			break
		}
	}
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return true
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", ociLocationForRequest(r, repository.Name, asset.Path))
	w.WriteHeader(http.StatusCreated)
	return true
}

func parseOCIContentRange(header string) (int64, int64, error) {
	fields := strings.Fields(header)
	var value string
	switch {
	case len(fields) == 1:
		// OCI Distribution 1.1 specifies the bare numeric form "start-end".
		value = fields[0]
	case len(fields) == 2 && strings.EqualFold(fields[0], "bytes"):
		// Retain the HTTP-style prefix accepted by earlier suxen versions and
		// emitted by some clients. It is unambiguous and safe to parse.
		value = fields[1]
	default:
		return 0, 0, fmt.Errorf("invalid Content-Range")
	}
	span, total, hasTotal := strings.Cut(value, "/")
	var totalSize int64
	if hasTotal && total != "*" {
		var err error
		if !ociRangeDigits(total) {
			return 0, 0, fmt.Errorf("invalid Content-Range")
		}
		totalSize, err = strconv.ParseInt(total, 10, 64)
		if err != nil || totalSize <= 0 {
			return 0, 0, fmt.Errorf("invalid Content-Range")
		}
	}
	startValue, endValue, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Content-Range")
	}
	if !ociRangeDigits(startValue) || !ociRangeDigits(endValue) {
		return 0, 0, fmt.Errorf("invalid Content-Range")
	}
	start, err := strconv.ParseInt(startValue, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid Content-Range")
	}
	end, err := strconv.ParseInt(endValue, 10, 64)
	if err != nil || end < start || end == math.MaxInt64 {
		return 0, 0, fmt.Errorf("invalid Content-Range")
	}
	if hasTotal && total != "*" {
		if totalSize <= end {
			return 0, 0, fmt.Errorf("invalid Content-Range")
		}
	}
	return start, end, nil
}

func ociRangeDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func ociRequestContentRange(r *http.Request) (int64, int64, bool, error) {
	values := r.Header.Values("Content-Range")
	if len(values) == 0 {
		return 0, 0, false, nil
	}
	if len(values) != 1 {
		return 0, 0, true, fmt.Errorf("invalid Content-Range")
	}
	start, end, err := parseOCIContentRange(values[0])
	return start, end, true, err
}

var errOCIUploadLengthMismatch = errors.New("OCI upload body length mismatch")

// ociUploadLengthReader reports an error before a storage driver can commit an
// append whose source ends early or contains bytes beyond the declared length.
// UploadStore.AppendUpload must leave the upload unchanged on source errors.
type ociUploadLengthReader struct {
	source    io.Reader
	remaining int64
	verified  bool
}

func (reader *ociUploadLengthReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if reader.verified {
		return 0, io.EOF
	}
	if reader.remaining == 0 {
		var extra [1]byte
		n, err := reader.source.Read(extra[:])
		if n != 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, errors.Join(errOCIUploadLengthMismatch, err)
			}
			return 0, errOCIUploadLengthMismatch
		}
		if errors.Is(err, io.EOF) {
			reader.verified = true
			return 0, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, errors.Join(errOCIUploadLengthMismatch, err)
		}
		return 0, err
	}
	if int64(len(p)) > reader.remaining {
		p = p[:int(reader.remaining)]
	}
	n, err := reader.source.Read(p)
	reader.remaining -= int64(n)
	if errors.Is(err, io.EOF) && reader.remaining != 0 {
		return n, errOCIUploadLengthMismatch
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return n, errors.Join(errOCIUploadLengthMismatch, err)
	}
	if errors.Is(err, io.EOF) && reader.remaining == 0 {
		reader.verified = true
	}
	return n, err
}

// preflightOCIUploadContentRange gives a prompt range response before capacity
// reservation. appendReservedOCIUpload repeats the authoritative offset check
// while holding the operation lease because this snapshot may immediately age.
func (h *Handler) preflightOCIUploadContentRange(
	w http.ResponseWriter,
	r *http.Request,
	state ociUploadState,
) bool {
	start, end, hasRange, rangeErr := ociRequestContentRange(r)
	if !hasRange {
		return true
	}
	if rangeErr != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", rangeErr.Error())
		return false
	}
	session, err := h.uploads().UploadSession(r.Context(), state.identity.ID)
	if err == nil && session.UploadSessionIdentity != state.identity {
		err = domain.ErrNotFound
	}
	if err != nil {
		writeOCIUploadLedgerError(w, err)
		return false
	}
	expected := end - start + 1
	if start != session.Size || r.ContentLength >= 0 && r.ContentLength != expected {
		writeOCIUploadHeaders(
			w, r, state.identity.Repository, state.identity.Image, state.identity.ID, session.Size,
		)
		httpx.WriteOCIError(
			w,
			http.StatusRequestedRangeNotSatisfiable,
			"BLOB_UPLOAD_INVALID",
			"Content-Range does not match the current upload offset",
		)
		return false
	}
	return true
}

func ociUploadStorageKey(repositoryName string, uploadID string) string {
	repositoryHash := sha256.Sum256([]byte(repositoryName))
	return fmt.Sprintf("oci/%x/%s", repositoryHash, uploadID)
}

func writeOCIUploadStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
		return
	}
	httpx.WriteOCIInternalError(w, err)
}

func writeOCIUploadHeaders(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	imageName string,
	uploadID string,
	size int64,
) {
	assetPath := fmt.Sprintf("v2/%s/blobs/uploads/%s", imageName, uploadID)
	location := ociLocationForRequest(r, repositoryName, assetPath)
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Location", location)
	if size == 0 {
		w.Header().Set("Range", "0-0")
	} else {
		w.Header().Set("Range", fmt.Sprintf("0-%d", size-1))
	}
}

func newUploadID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("read cryptographic randomness: %v", err))
	}
	return hex.EncodeToString(bytes)
}

func validUploadID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
