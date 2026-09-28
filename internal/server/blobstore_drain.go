package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// blobStoreAdmin is the blob-store lifecycle capability the bootstrap, drain,
// and migration paths need: create the default store at startup, begin a drain,
// set a store's state/drain target, and rebind repositories from a drained store
// to its target. These carry physical or cross-store effects and stay outside
// the control-plane ownership service.
type blobStoreAdmin interface {
	CreateBlobStore(context.Context, domain.BlobStore) error
	BeginBlobStoreDrain(ctx context.Context, source string, target string) error
	SetBlobStoreState(ctx context.Context, name string, state string, drainTarget string) error
	RebindRepositories(ctx context.Context, from string, to string) (int64, error)
}

// blobStoreAdmin narrows the metadata store to the blob-store lifecycle
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) blobStoreAdmin() blobStoreAdmin {
	return s.metadata
}

type blobStoreDrainRequest struct {
	Target string `json:"target"`
}

// handleBlobStoreDrain starts (POST) or clears (DELETE) a blob store's drain.
// Draining marks a store for migration onto a target and blocks new repository
// bindings to it; clearing the drain is reversible and keeps every blob.
func (s *Server) handleBlobStoreDrain(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodPost:
		s.drainBlobStore(w, r, name)
	case http.MethodDelete:
		s.clearBlobStoreDrain(w, r, name)
	default:
		httpx.MethodNotAllowed(w, http.MethodPost, http.MethodDelete)
	}
}

func (s *Server) drainBlobStore(w http.ResponseWriter, r *http.Request, name string) {
	var request blobStoreDrainRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	source, err := s.blobStoreReads().BlobStore(r.Context(), name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if name == "default" || source.State != domain.BlobStoreStateActive {
		httpx.WriteResult(w, nil, domain.ErrBlobStoreNotDrainable)
		return
	}
	if request.Target == "" || request.Target == name {
		httpx.WriteResult(w, nil, domain.ErrInvalidDrainTarget)
		return
	}
	target, err := s.blobStoreReads().BlobStore(r.Context(), request.Target)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.WriteResult(w, nil, domain.ErrInvalidDrainTarget)
			return
		}
		httpx.WriteResult(w, nil, err)
		return
	}
	if target.State != domain.BlobStoreStateActive {
		httpx.WriteResult(w, nil, domain.ErrInvalidDrainTarget)
		return
	}
	err = s.content.WithBlobStoreLease(r.Context(), name, func(leaseCtx context.Context) error {
		return s.blobStoreAdmin().BeginBlobStoreDrain(leaseCtx, name, request.Target)
	})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	s.log.Info("blob store draining", "blobStore", name, "target", request.Target)
	s.writeBlobStoreResource(w, r, name)
}

func (s *Server) clearBlobStoreDrain(w http.ResponseWriter, r *http.Request, name string) {
	if name == "default" {
		httpx.WriteResult(w, nil, domain.ErrDefaultBlobStoreImmutable)
		return
	}
	source, err := s.blobStoreReads().BlobStore(r.Context(), name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	// Idempotent: an already-active store needs no write. Clearing the drain of
	// a drained store returns it to active with whatever blobs remain.
	if source.State != domain.BlobStoreStateActive {
		err := s.content.WithBlobStoreLease(r.Context(), name, func(leaseCtx context.Context) error {
			return s.blobStoreAdmin().SetBlobStoreState(
				leaseCtx, name, domain.BlobStoreStateActive, "",
			)
		})
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		s.log.Info("blob store drain cleared", "blobStore", name)
	}
	s.writeBlobStoreResource(w, r, name)
}

func (s *Server) writeBlobStoreResource(w http.ResponseWriter, r *http.Request, name string) {
	blobStore, err := s.blobStoreReads().BlobStore(r.Context(), name)
	if err == nil {
		blobStore.Managed, err = s.resourceIsManaged(r.Context(), "blobStore", name)
	}
	httpx.WriteResult(w, blobStore, err)
}
