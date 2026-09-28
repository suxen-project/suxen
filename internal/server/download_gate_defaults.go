package server

import (
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// defaultsProvisionName is the provisioning-record name of every singleton
// instance-wide default. It matches the reserved resource name a provisioning
// document uses on the base kind, so a default provisioned at deploy time is
// recognized as managed here.
const defaultsProvisionName = domain.InstanceDefaultsName

// handleDownloadGateDefaults serves the instance-wide download-gate default that
// inheriting repositories AND-extend. It is a singleton: there is no name path
// parameter, and DELETE clears it back to "no default". A default owned by
// declarative provisioning is read-only here: PUT/DELETE return 409 unless the
// caller passes ?force=true, which also releases provisioning ownership.
func (s *Server) handleDownloadGateDefaults(w http.ResponseWriter, r *http.Request) {
	const kind = "downloadGate"
	switch r.Method {
	case http.MethodGet:
		gate, err := s.downloadGateReads().DownloadGateDefaults(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		gate.Managed, err = s.resourceIsManaged(r.Context(), kind, defaultsProvisionName)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, gate)
	case http.MethodPut:
		if !s.rejectManagedMutation(w, r, kind, defaultsProvisionName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		_, existingErr := s.downloadGateReads().DownloadGateDefaults(r.Context())
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
		if err := s.downloadGates.SaveDownloadGateDefaults(r.Context(), controlplane.SaveDownloadGateDefaultsCommand{
			Gate:   request.domainDefaults(),
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.downloadGateReads().DownloadGateDefaults(r.Context())
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
		if err := s.downloadGates.DeleteDownloadGateDefaults(
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
