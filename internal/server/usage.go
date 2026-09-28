package server

import (
	"net/http"

	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// reports narrows the metadata store to the instance reporting capability
// (aggregate stats and per-repository/per-store storage usage) — the same
// store.MetricsMetadata contract the metric collectors use. It reads s.metadata
// on each call so a reconfigured backend is honoured.
func (s *Server) reports() store.MetricsMetadata {
	return s.metadata
}

// handleStorageUsage reports deduplicated blob bytes per repository and per blob
// store. Per-repository figures are served here on demand rather than as
// Prometheus series because repository cardinality is unbounded; per-blob-store
// figures are additionally exported as bounded gauges.
func (s *Server) handleStorageUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	usage, err := s.reports().StorageUsage(r.Context())
	httpx.WriteResult(w, usage, err)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	stats, err := s.reports().Stats(r.Context())
	httpx.WriteResult(w, stats, err)
}
