package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// trustPolicyCommands is the trust-policy mutation capability the trust-policy
// handlers consume from the control plane: per-repository and instance-default
// create/replace and delete, each folding the ownership record into one
// transaction.
type trustPolicyCommands interface {
	SaveTrustPolicy(context.Context, controlplane.SaveTrustPolicyCommand) error
	DeleteTrustPolicy(context.Context, string, controlplane.Intent) error
	SaveTrustPolicyDefaults(context.Context, controlplane.SaveTrustPolicyDefaultsCommand) error
	DeleteTrustPolicyDefaults(context.Context, controlplane.Intent) error
}

// trustPolicyReads is the trust-policy read capability the trust-policy
// handlers need: a repository's configured policy by name and the instance-wide
// default. The repository-scoped effective (merged) policy is a separate read
// used elsewhere.
type trustPolicyReads interface {
	TrustPolicy(context.Context, string) (domain.TrustPolicy, error)
	TrustPolicyDefaults(context.Context) (domain.TrustPolicy, error)
}

// trustPolicyReads narrows the metadata store to the trust-policy read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) trustPolicyReads() trustPolicyReads {
	return s.metadata
}

type trustPolicyRequest struct {
	Mode                   string                 `json:"mode"`
	PublicKeys             []string               `json:"publicKeys,omitempty"`
	CertificateAuthorities []string               `json:"certificateAuthorities,omitempty"`
	AllowedIdentities      []domain.TrustIdentity `json:"allowedIdentities,omitempty"`
	DeniedFingerprints     []string               `json:"deniedFingerprints,omitempty"`
}

func (request trustPolicyRequest) domainPolicy(repositoryName string) domain.TrustPolicy {
	return domain.TrustPolicy{
		Repository:             repositoryName,
		Mode:                   request.Mode,
		PublicKeys:             request.PublicKeys,
		CertificateAuthorities: request.CertificateAuthorities,
		AllowedIdentities:      request.AllowedIdentities,
		DeniedFingerprints:     request.DeniedFingerprints,
	}
}

func (request trustPolicyRequest) domainDefaults() domain.TrustPolicy {
	return request.domainPolicy("")
}

func (s *Server) handleTrustPolicy(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if !s.requireRepositoryResource(w, r, repositoryName) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		policy, err := s.trustPolicyReads().TrustPolicy(r.Context(), repositoryName)
		if err == nil {
			policy.Managed, err = s.resourceIsManaged(r.Context(), "trustPolicy", repositoryName)
		}
		httpx.WriteResult(w, policy, err)
	case http.MethodPut:
		repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if repository.Type == "group" {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_trust_policy",
				"group repositories do not support trust policies; configure policies on their members")
			return
		}
		_, existingErr := s.trustPolicyReads().TrustPolicy(r.Context(), repositoryName)
		creating := errors.Is(existingErr, domain.ErrNotFound)
		if existingErr != nil && !creating {
			httpx.WriteResult(w, nil, existingErr)
			return
		}
		var request trustPolicyRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if !s.rejectManagedMutation(w, r, "trustPolicy", repositoryName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		policy := request.domainPolicy(repositoryName)
		if err := content.ValidateTrustPolicyMaterial(policy); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if policy.Mode == "verify-on-push" {
			if repository.Type != "hosted" {
				httpx.WriteProblem(
					w,
					http.StatusBadRequest,
					"invalid_enforcement",
					"verify-on-push is only valid for hosted repositories",
				)
				return
			}
		}
		if err := s.trustPolicies.SaveTrustPolicy(r.Context(), controlplane.SaveTrustPolicyCommand{
			Policy: policy,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.trustPolicyReads().TrustPolicy(r.Context(), repositoryName)
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
		if err := s.trustPolicies.DeleteTrustPolicy(
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

func (s *Server) handleAssetVerification(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	assetIDValue string,
) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}
	assetID, err := strconv.ParseInt(assetIDValue, 10, 64)
	if err != nil || assetID <= 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_id", "asset ID must be positive")
		return
	}
	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	metadata := s.metadata.ForRepository(repository)
	asset, err := metadata.AssetByID(r.Context(), assetID)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	policy, err := metadata.EffectiveTrustPolicy(r.Context())
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	var request domain.VerificationRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	result := content.VerifyProvenance(policy, asset, request)
	s.content.RecordProvenanceMetric(result)
	if err := s.content.RecordProvenance(r.Context(), asset, result); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if result.Status != "passed" {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, result)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}
