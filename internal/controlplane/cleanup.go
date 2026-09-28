package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// CleanupPolicyStore is the backend capability the cleanup-policy service
// consumes: the atomic policy mutations that fold the ownership record into one
// transaction serialized on the policy key. The SQL store satisfies it.
type CleanupPolicyStore interface {
	SaveCleanupPolicy(context.Context, store.CleanupPolicySave) error
	DeleteCleanupPolicy(context.Context, string, store.Ownership) error
}

// SaveCleanupPolicyCommand creates or updates a reusable cleanup policy.
type SaveCleanupPolicyCommand struct {
	Policy domain.CleanupPolicy
	Create bool
	Intent Intent
}

// CleanupPolicyService applies cleanup-policy mutations and their ownership
// effects atomically. It is stateless beyond its backend.
type CleanupPolicyService struct {
	backend CleanupPolicyStore
}

// NewCleanupPolicyService composes the cleanup-policy command service over its backend.
func NewCleanupPolicyService(backend CleanupPolicyStore) *CleanupPolicyService {
	return &CleanupPolicyService{backend: backend}
}

// SaveCleanupPolicy applies a cleanup-policy mutation and its ownership effect
// atomically. It returns domain.ErrManaged when an imperative caller targets a
// provisioning-managed policy without Force.
func (s *CleanupPolicyService) SaveCleanupPolicy(ctx context.Context, cmd SaveCleanupPolicyCommand) error {
	return s.backend.SaveCleanupPolicy(ctx, store.CleanupPolicySave{
		Policy:    cmd.Policy,
		Create:    cmd.Create,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteCleanupPolicy removes a cleanup policy and its ownership record atomically.
func (s *CleanupPolicyService) DeleteCleanupPolicy(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteCleanupPolicy(ctx, name, intent.ownership())
}
