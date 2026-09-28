package server

import (
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// handleTrustPolicyDefaults serves the instance-wide trust-policy default that a
// repository inherits when it has no policy of its own. It is a singleton: there
// is no name path parameter, and a repository policy overrides it entirely. A
// default owned by declarative provisioning is read-only here: PUT/DELETE return
// 409 unless the caller passes ?force=true, which also releases ownership.
func (s *Server) handleTrustPolicyDefaults(w http.ResponseWriter, r *http.Request) {
	const kind = "trustPolicy"
	switch r.Method {
	case http.MethodGet:
		policy, err := s.trustPolicyReads().TrustPolicyDefaults(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		policy.Managed, err = s.resourceIsManaged(r.Context(), kind, defaultsProvisionName)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, policy)
	case http.MethodPut:
		if !s.rejectManagedMutation(w, r, kind, defaultsProvisionName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		_, existingErr := s.trustPolicyReads().TrustPolicyDefaults(r.Context())
		creating := errors.Is(existingErr, domain.ErrNotFound)
		if existingErr != nil && !creating {
			httpx.WriteResult(w, nil, existingErr)
			return
		}
		var request trustPolicyRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		policy := request.domainDefaults()
		if err := content.ValidateTrustPolicyDefaultsMaterial(policy); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.trustPolicies.SaveTrustPolicyDefaults(r.Context(), controlplane.SaveTrustPolicyDefaultsCommand{
			Policy: policy,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.trustPolicyReads().TrustPolicyDefaults(r.Context())
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
		if err := s.trustPolicies.DeleteTrustPolicyDefaults(
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
