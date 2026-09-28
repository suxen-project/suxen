package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// TrustPolicyStore is the backend capability the trust-policy service consumes:
// the atomic per-repository and instance-default policy mutations that fold the
// ownership record into one transaction serialized on the policy key. The SQL
// store satisfies it.
type TrustPolicyStore interface {
	SaveTrustPolicy(context.Context, store.TrustPolicySave) error
	DeleteTrustPolicy(context.Context, string, store.Ownership) error
	SaveTrustPolicyDefaults(context.Context, store.TrustPolicyDefaultsSave) error
	DeleteTrustPolicyDefaults(context.Context, store.Ownership) error
}

// SaveTrustPolicyCommand creates or replaces a repository signature policy.
type SaveTrustPolicyCommand struct {
	Policy domain.TrustPolicy
	Intent Intent
}

// SaveTrustPolicyDefaultsCommand creates or replaces the instance-wide
// trust-policy default.
type SaveTrustPolicyDefaultsCommand struct {
	Policy domain.TrustPolicy
	Intent Intent
}

// TrustPolicyService applies trust-policy mutations and their ownership effects
// atomically. It is stateless beyond its backend.
type TrustPolicyService struct {
	backend TrustPolicyStore
}

// NewTrustPolicyService composes the trust-policy command service over its backend.
func NewTrustPolicyService(backend TrustPolicyStore) *TrustPolicyService {
	return &TrustPolicyService{backend: backend}
}

// SaveTrustPolicy applies a repository trust-policy mutation and its ownership
// effect atomically.
func (s *TrustPolicyService) SaveTrustPolicy(ctx context.Context, cmd SaveTrustPolicyCommand) error {
	return s.backend.SaveTrustPolicy(ctx, store.TrustPolicySave{
		Policy:    cmd.Policy,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteTrustPolicy removes a repository trust policy and its ownership record atomically.
func (s *TrustPolicyService) DeleteTrustPolicy(ctx context.Context, repository string, intent Intent) error {
	return s.backend.DeleteTrustPolicy(ctx, repository, intent.ownership())
}

// SaveTrustPolicyDefaults applies an instance-wide trust-policy-default mutation
// and its ownership effect atomically.
func (s *TrustPolicyService) SaveTrustPolicyDefaults(ctx context.Context, cmd SaveTrustPolicyDefaultsCommand) error {
	return s.backend.SaveTrustPolicyDefaults(ctx, store.TrustPolicyDefaultsSave{
		Policy:    cmd.Policy,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteTrustPolicyDefaults removes the instance-wide trust-policy default and its ownership record atomically.
func (s *TrustPolicyService) DeleteTrustPolicyDefaults(ctx context.Context, intent Intent) error {
	return s.backend.DeleteTrustPolicyDefaults(ctx, intent.ownership())
}
