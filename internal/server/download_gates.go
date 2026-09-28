package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// downloadGateCommands is the download-gate mutation capability the download-gate
// handlers consume from the control plane: per-repository and instance-default
// create/replace and delete, each folding the ownership record into one
// transaction.
type downloadGateCommands interface {
	SaveDownloadGate(context.Context, controlplane.SaveDownloadGateCommand) error
	DeleteDownloadGate(context.Context, string, controlplane.Intent) error
	SaveDownloadGateDefaults(context.Context, controlplane.SaveDownloadGateDefaultsCommand) error
	DeleteDownloadGateDefaults(context.Context, controlplane.Intent) error
}

// downloadGateReads is the download-gate read capability the download-gate
// handlers need: a repository's gate by name and the instance-wide default.
type downloadGateReads interface {
	DownloadGate(context.Context, string) (domain.DownloadGate, error)
	DownloadGateDefaults(context.Context) (domain.DownloadGate, error)
}

// downloadGateReads narrows the metadata store to the download-gate read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) downloadGateReads() downloadGateReads {
	return s.metadata
}

type downloadGateRequest struct {
	Criteria      []domain.Predicate `json:"criteria"`
	Enabled       *bool              `json:"enabled,omitempty"`
	InheritGlobal *bool              `json:"inheritGlobal,omitempty"`
}

func (request downloadGateRequest) domainGate(repositoryName string) domain.DownloadGate {
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	inheritGlobal := true
	if request.InheritGlobal != nil {
		inheritGlobal = *request.InheritGlobal
	}
	return domain.DownloadGate{
		Repository:    repositoryName,
		Criteria:      request.Criteria,
		Enabled:       enabled,
		InheritGlobal: inheritGlobal,
	}
}

func (request downloadGateRequest) domainDefaults() domain.DownloadGate {
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	return domain.DownloadGate{
		Criteria: request.Criteria,
		Enabled:  enabled,
	}
}

func (s *Server) handleDownloadGate(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if !s.requireRepositoryResource(w, r, repositoryName) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		gate, err := s.downloadGateReads().DownloadGate(r.Context(), repositoryName)
		if err == nil {
			gate.Managed, err = s.resourceIsManaged(r.Context(), "downloadGate", repositoryName)
		}
		httpx.WriteResult(w, gate, err)
	case http.MethodPut:
		_, existingErr := s.downloadGateReads().DownloadGate(r.Context(), repositoryName)
		creating := errors.Is(existingErr, domain.ErrNotFound)
		if existingErr != nil && !creating {
			httpx.WriteResult(w, nil, existingErr)
			return
		}
		var request downloadGateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if request.Criteria == nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", "criteria is required and must be an array")
			return
		}
		if !s.rejectManagedMutation(w, r, "downloadGate", repositoryName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		gate := request.domainGate(repositoryName)
		if err := s.downloadGates.SaveDownloadGate(r.Context(), controlplane.SaveDownloadGateCommand{
			Gate:   gate,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.downloadGateReads().DownloadGate(r.Context(), repositoryName)
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
		if err := s.downloadGates.DeleteDownloadGate(
			r.Context(), repositoryName, controlplane.Imperative(force),
		); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}
