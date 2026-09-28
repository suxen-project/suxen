package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// downloadPermitted evaluates the download gate and trust policy for an asset
// and writes the refusal when it fails. It reports whether serving may go on.
func (rt *Runtime) downloadPermitted(w http.ResponseWriter, r *http.Request, asset domain.Asset) bool {
	denial, err := rt.DownloadAllowed(r.Context(), asset)
	// A publisher may fetch a digest-addressed manifest to create its
	// signature. That exception only waives provenance; the download gate
	// remains an independent repository policy.
	if err == nil && denial == "provenance_required" &&
		asset.Kind == "oci-manifest" &&
		strings.HasPrefix(asset.Reference, "sha256:") &&
		rt.publisherSigningReadAllowed(r, asset.Repository) {
		denial, err = rt.downloadGateAllowed(r.Context(), asset)
	}
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"download_gate_error",
			"evaluate download gate failed",
			err,
		)
		return false
	}
	if denial != "" {
		detail := "asset attributes do not satisfy the repository download gate"
		if denial == "provenance_required" {
			detail = "asset provenance does not satisfy the repository trust policy"
		}
		httpx.WriteProblem(w, http.StatusForbidden, denial, detail)
		return false
	}
	return true
}

func (rt *Runtime) publisherSigningReadAllowed(r *http.Request, repository string) bool {
	if rt.Auth == nil {
		return false
	}
	user, authenticated := rt.Auth.Authenticate(r)
	if !authenticated {
		return false
	}
	allowed, err := rt.Auth.UserHasPrivilege(
		r, user, true, "repository:"+repository+":write",
	)
	return err == nil && allowed
}

func (rt *Runtime) ServeAsset(w http.ResponseWriter, r *http.Request, asset domain.Asset) {
	if !rt.downloadPermitted(w, r, asset) {
		return
	}

	reader, info, err := rt.OpenStoredAsset(r.Context(), asset)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The metadata references this blob but the store no longer holds it:
			// a dangling reference an operator must reconcile, logged with the
			// coordinates needed to locate it, not a plain client 404.
			rt.requestLogger(r).Warn(
				"referenced blob missing from store",
				"repository", asset.Repository,
				"blobStore", asset.BlobStore,
				"digest", asset.Digest,
				"path", asset.Path,
				"asset", asset.ID,
			)
		}
		httpx.WriteResult(w, nil, err)
		return
	}
	defer reader.Close()

	start := int64(0)
	end := info.Size - 1
	status := http.StatusOK
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", asset.ContentType)
	// Stored artifact media types are uploader-controlled. Force downloads into
	// their own browsing context and sandbox any client that still renders an
	// active type, so artifact bytes cannot execute with the control-plane
	// origin's authority. Synthesized package indexes use a separate response
	// path and remain renderable by package clients.
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Docker-Content-Digest", asset.Digest)
	w.Header().Set("ETag", quoteETag(asset.Digest))
	w.Header().Set("Last-Modified", asset.UpdatedAt.Format(http.TimeFormat))
	if !checkReadPreconditions(w, r, asset.UpdatedAt) {
		return
	}

	if rangeHeader := r.Header.Get("Range"); r.Method == http.MethodGet && rangeHeader != "" &&
		ifRangeMatches(r.Header.Get("If-Range"), quoteETag(asset.Digest)) {
		rangeStart, rangeEnd, err := parseSingleByteRange(rangeHeader, info.Size)
		if err == errUnsatisfiableRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
			httpx.WriteProblem(
				w,
				http.StatusRequestedRangeNotSatisfiable,
				"range_not_satisfiable",
				"requested range is not satisfiable",
			)
			return
		}
		if err == nil {
			start = rangeStart
			end = rangeEnd
			status = http.StatusPartialContent
			w.Header().Set(
				"Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size),
			)
		}
	}

	length := int64(0)
	if info.Size > 0 && end >= start {
		length = end - start + 1
	}
	w.Header().Set("Content-Length", fmt.Sprint(length))

	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	if start > 0 {
		if _, err := io.CopyN(io.Discard, reader, start); err != nil {
			httpx.WriteServerProblem(
				w,
				http.StatusInternalServerError,
				"internal_error",
				"failed to seek to requested range",
				err,
			)
			return
		}
	}

	w.WriteHeader(status)
	if length > 0 {
		if _, err := io.CopyN(w, reader, length); err != nil {
			rt.requestLogger(r).Warn("asset stream interrupted", "asset", asset.ID, "error", err)
			return
		}
	}

	rt.recordAssetDownload(r, asset)
}

func (rt *Runtime) recordAssetDownload(r *http.Request, asset domain.Asset) {
	rt.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		view := rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID})
		if err := view.TouchAsset(ctx, asset.ID, time.Now()); err != nil {
			rt.requestLogger(r).Warn("update last download time", "asset", asset.ID, "error", err)
		}
		rt.EnqueueAssetEvent(ctx, domain.WebhookAssetDownloaded, asset)
	})
}

var (
	// errUnsatisfiableRange is returned when a Range header cannot be satisfied.
	errUnsatisfiableRange = fmt.Errorf("unsatisfiable range")
	errIgnoreRange        = fmt.Errorf("ignore range")
)

func parseSingleByteRange(header string, size int64) (int64, int64, error) {
	header = strings.TrimSpace(header)
	unit, spec, ok := strings.Cut(header, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return 0, 0, errIgnoreRange
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, errIgnoreRange
	}
	if size <= 0 {
		return 0, 0, errUnsatisfiableRange
	}

	startValue, endValue, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errUnsatisfiableRange
	}
	startValue = strings.TrimSpace(startValue)
	endValue = strings.TrimSpace(endValue)

	if startValue == "" {
		suffix, err := strconv.ParseInt(endValue, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, errUnsatisfiableRange
		}
		if suffix >= size {
			return 0, size - 1, nil
		}
		return size - suffix, size - 1, nil
	}

	start, err := strconv.ParseInt(startValue, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, errUnsatisfiableRange
	}
	if start >= size {
		return 0, 0, errUnsatisfiableRange
	}

	end := size - 1
	if endValue != "" {
		end, err = strconv.ParseInt(endValue, 10, 64)
		if err != nil || end < start {
			return 0, 0, errUnsatisfiableRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, nil
}
