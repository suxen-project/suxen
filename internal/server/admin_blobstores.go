package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

type blobStoreResourceRequest struct {
	Name             string                         `json:"name"`
	Driver           string                         `json:"driver"`
	ConfigurationRef *domain.ConfigurationReference `json:"configurationRef"`
	Attributes       map[string]any                 `json:"attributes,omitempty"`
}

type blobStoreUpdateRequest struct {
	Driver           string                         `json:"driver"`
	ConfigurationRef *domain.ConfigurationReference `json:"configurationRef"`
	Attributes       map[string]any                 `json:"attributes,omitempty"`
}

func (request blobStoreUpdateRequest) domainBlobStore(name string) domain.BlobStore {
	return blobStoreResourceRequest{
		Driver:           request.Driver,
		ConfigurationRef: request.ConfigurationRef,
		Attributes:       request.Attributes,
	}.domainBlobStore(name)
}

func (request blobStoreResourceRequest) domainBlobStore(name string) domain.BlobStore {
	if name == "" {
		name = request.Name
	}
	return domain.BlobStore{
		Name:             name,
		Driver:           request.Driver,
		ConfigurationRef: request.ConfigurationRef,
		Attributes:       request.Attributes,
	}
}

func (s *Server) handleBlobStoreCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		blobStores, err := s.blobStoreReads().BlobStores(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "blobStore")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range blobStores {
			blobStores[index].Managed = managedName(managed, blobStores[index].Name)
		}
		httpx.WriteCollection(w, r, "blob-stores", blobStores, func(blobStore domain.BlobStore) string {
			return blobStore.Name
		})
	case http.MethodPost:
		var request blobStoreResourceRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		blobStore := request.domainBlobStore("")
		prepared, opened, err := s.blobStores.Prepare(r.Context(), blobStore)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.blobStoreCommands.SaveBlobStore(r.Context(), controlplane.SaveBlobStoreCommand{
			BlobStore: prepared,
			Create:    true,
			Intent:    controlplane.Imperative(false),
		}); err != nil {
			content.CloseStore(opened)
			httpx.WriteResult(w, nil, err)
			return
		}
		s.blobStores.Remember(prepared, opened)
		created, err := s.blobStoreReads().BlobStore(r.Context(), blobStore.Name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleBlobStoreItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		blobStore, err := s.blobStoreReads().BlobStore(r.Context(), name)
		if err == nil {
			blobStore.Managed, err = s.resourceIsManaged(r.Context(), "blobStore", name)
		}
		httpx.WriteResult(w, blobStore, err)
	case http.MethodPut:
		var request blobStoreUpdateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		blobStore := request.domainBlobStore(name)
		prepared, configuration, err := content.ResolveResource(blobStore)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		_, err = s.blobStoreReads().BlobStore(r.Context(), name)
		creating := errors.Is(err, domain.ErrNotFound)
		if err != nil && !creating {
			httpx.WriteResult(w, nil, err)
			return
		}
		if !creating {
			// An existing store commits the editable attributes and its ownership
			// record together. The store enforces the immutable-definition and
			// managed-resource guards and no-ops an identical definition, so no
			// backend is opened for an attribute-only or rejected update.
			if err := s.blobStoreCommands.SaveBlobStore(r.Context(), controlplane.SaveBlobStoreCommand{
				BlobStore: prepared,
				Create:    false,
				Intent:    controlplane.Imperative(force),
			}); err != nil {
				httpx.WriteResult(w, nil, err)
				return
			}
			updated, err := s.blobStoreReads().BlobStore(r.Context(), name)
			httpx.WriteResult(w, updated, err)
			return
		}
		prepared, opened, err := s.blobStores.CheckReady(
			r.Context(),
			prepared,
			configuration,
		)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.blobStoreCommands.SaveBlobStore(r.Context(), controlplane.SaveBlobStoreCommand{
			BlobStore: prepared,
			Create:    true,
			Intent:    controlplane.Imperative(force),
		}); err != nil {
			content.CloseStore(opened)
			httpx.WriteResult(w, nil, err)
			return
		}
		s.blobStores.Remember(prepared, opened)
		updated, err := s.blobStoreReads().BlobStore(r.Context(), name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, r.URL.Path, updated)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.content.WithBlobStoreLease(r.Context(), name, func(leaseCtx context.Context) error {
			if err := s.blobStoreCommands.DeleteBlobStore(leaseCtx, name, controlplane.Imperative(force)); err != nil {
				return err
			}
			s.blobStores.Forget(name)
			return nil
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func blobStoreResourcesEqual(left domain.BlobStore, right domain.BlobStore) bool {
	return blobStoreRuntimeConfigurationEqual(left, right) &&
		reflect.DeepEqual(left.Attributes, right.Attributes)
}

func blobStoreRuntimeConfigurationEqual(
	left domain.BlobStore,
	right domain.BlobStore,
) bool {
	if left.Driver != right.Driver || left.PhysicalIdentity != right.PhysicalIdentity {
		return false
	}
	if left.ConfigurationRef == nil || right.ConfigurationRef == nil {
		return left.ConfigurationRef == nil && right.ConfigurationRef == nil
	}
	return *left.ConfigurationRef == *right.ConfigurationRef
}

// blobStoreCommands is the metadata half of a blob-store mutation the HTTP
// handlers and the provisioning controller need from the control plane:
// create/update and delete, each folding the ownership record into one
// transaction. Physical backend readiness and the in-memory backend cache stay
// with the caller.
type blobStoreCommands interface {
	SaveBlobStore(context.Context, controlplane.SaveBlobStoreCommand) error
	DeleteBlobStore(context.Context, string, controlplane.Intent) error
}

// blobStoreReads is the blob-store read capability the handlers, the drain and
// usage reporting, gc, verification, and bootstrap need: one store by name and
// the full list. Blob-store lifecycle mutations stay on their own ports.
type blobStoreReads interface {
	BlobStore(context.Context, string) (domain.BlobStore, error)
	BlobStores(context.Context) ([]domain.BlobStore, error)
}

// blobStoreReads narrows the metadata store to the blob-store read capability.
// It reads s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) blobStoreReads() blobStoreReads {
	return s.metadata
}
