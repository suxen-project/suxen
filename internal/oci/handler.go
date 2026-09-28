// Package oci implements the OCI Distribution HTTP API. It depends on
// content.Runtime and identity.Service and does not import server.
package oci

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// Handler serves /v2 and repository-scoped OCI routes.
type Handler struct {
	Runtime                    *content.Runtime
	uploadLeaseDurationForTest time.Duration
}

// SetUploadLeaseDurationForTest shortens the internal lease in integration
// tests so heartbeat and expiry behavior can be exercised without minutes of
// wall time. It must be called before serving requests.
func (h *Handler) SetUploadLeaseDurationForTest(duration time.Duration) {
	h.uploadLeaseDurationForTest = duration
}

// New constructs an OCI handler over the shared data-plane runtime.
func New(runtime *content.Runtime) *Handler {
	return &Handler{Runtime: runtime}
}

// meta and uploads are the narrow backend capabilities the handler uses, handed
// over by the runtime as store ports so the handler never receives store.Store.
// They read the runtime's current backend on each call so a runtime SetMetadata
// (startup wiring, tests) is observed, keeping one source of truth rather than a
// captured copy that could diverge.
func (h *Handler) meta() store.OCIMetadata {
	return h.Runtime.OCIMetadata()
}

func (h *Handler) uploads() store.UploadLedger {
	return h.Runtime.UploadLedger()
}

func (h *Handler) metaFor(repository domain.Repository) store.RepositoryView {
	return h.Runtime.OCIRepositoryView(repository)
}

func (h *Handler) requestLogger(r *http.Request) *slog.Logger {
	return httpx.RequestLogger(h.Runtime.Log, r)
}
