package server

import (
	"net/http"
	"strconv"

	"github.com/suxen-project/suxen/internal/httpx"
)

// handleBlobStoreGC runs garbage collection scoped to one blob store. Collection
// only deletes blobs that metadata no longer references, and is safe per store
// because each physical store's digests are isolated from the others. It keeps
// the admin:gc:run privilege, not blob-stores:write, because it deletes stored
// data rather than blob-store configuration.
func (s *Server) handleBlobStoreGC(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}
	dryRun, err := strconv.ParseBool(defaultString(r.URL.Query().Get("dryRun"), "true"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_dry_run", err.Error())
		return
	}
	gracePeriod, err := parseGracePeriod(r.URL.Query().Get("grace"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_grace_period", err.Error())
		return
	}
	// Resolve the store first so an unknown name is a 404 and no task is created.
	if _, err := s.blobStoreReads().BlobStore(r.Context(), name); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	result, err := s.runGarbageCollection(r.Context(), dryRun, gracePeriod, name)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"gc_failed",
			"garbage collection failed",
			err,
		)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}
