package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// classificationCommands is the classification mutation capability the
// classification handlers consume from the control plane: per-repository and
// instance-default create/replace and delete, each folding the ownership record
// into one transaction.
type classificationCommands interface {
	SaveClassification(context.Context, controlplane.SaveClassificationCommand) error
	DeleteClassification(context.Context, string, controlplane.Intent) error
	SaveClassificationDefaults(context.Context, controlplane.SaveClassificationDefaultsCommand) error
	DeleteClassificationDefaults(context.Context, controlplane.Intent) error
}

// classificationReads is the classification read capability the classification
// handlers need: a repository's config by name and the instance-wide default.
type classificationReads interface {
	Classification(context.Context, string) (domain.ClassificationConfig, error)
	ClassificationDefaults(context.Context) (domain.ClassificationConfig, error)
}

// classificationReads narrows the metadata store to the classification read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) classificationReads() classificationReads {
	return s.metadata
}

// handleClassificationDefaults serves the instance-wide classification default
// that inheriting repositories prepend to their own rules. It is a singleton:
// there is no name path parameter, and writing it relabels every inheriting
// repository's assets. A default owned by declarative provisioning is read-only
// here: PUT/DELETE return 409 unless the caller passes ?force=true, which also
// releases provisioning ownership.
func (s *Server) handleClassificationDefaults(w http.ResponseWriter, r *http.Request) {
	const kind = "classification"
	switch r.Method {
	case http.MethodGet:
		config, err := s.classificationReads().ClassificationDefaults(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		config.Managed, err = s.resourceIsManaged(r.Context(), kind, defaultsProvisionName)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, config)
	case http.MethodPut:
		if !s.rejectManagedMutation(w, r, kind, defaultsProvisionName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		_, existingErr := s.classificationReads().ClassificationDefaults(r.Context())
		creating := errors.Is(existingErr, domain.ErrNotFound)
		if existingErr != nil && !creating {
			httpx.WriteResult(w, nil, existingErr)
			return
		}
		var request classificationRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if err := s.classifications.SaveClassificationDefaults(r.Context(), controlplane.SaveClassificationDefaultsCommand{
			Config: request.domainDefaults(),
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.classificationReads().ClassificationDefaults(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored.Managed, err = s.resourceIsManaged(r.Context(), kind, defaultsProvisionName)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if creating {
			httpx.WriteCreated(w, r.URL.Path, stored)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, stored)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.classifications.DeleteClassificationDefaults(
			r.Context(), controlplane.Imperative(force),
		); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}
