package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// RepositoryStore is the narrow backend port the repository feature service
// needs. The SQL store satisfies it; the service never receives HTTP or
// provisioning-document types.
type RepositoryStore interface {
	SaveRepository(context.Context, store.RepositorySave) error
	DeleteRepository(context.Context, string, store.Ownership) error
}

// SaveRepositoryCommand creates or replaces a repository definition. Create
// inserts a new repository; otherwise the definition is replaced in place.
type SaveRepositoryCommand struct {
	Repository domain.Repository
	Create     bool
	// PreserveUpstream retains the stored URL and credentials on update.
	PreserveUpstream bool
	Intent           Intent
}

// RepositoryService applies repository mutations and their ownership effect
// atomically. It is stateless beyond its store port and safe to construct per
// call site.
type RepositoryService struct {
	backend RepositoryStore
}

// NewRepositoryService composes the service over the given store port.
func NewRepositoryService(backend RepositoryStore) *RepositoryService {
	return &RepositoryService{backend: backend}
}

// SaveRepository applies a repository mutation and its ownership effect in one
// transaction.
func (s *RepositoryService) SaveRepository(ctx context.Context, cmd SaveRepositoryCommand) error {
	return s.backend.SaveRepository(ctx, store.RepositorySave{
		Repository:       cmd.Repository,
		Create:           cmd.Create,
		PreserveUpstream: cmd.PreserveUpstream,
		Ownership:        cmd.Intent.ownership(),
	})
}

// DeleteRepository removes a repository and its ownership record atomically.
func (s *RepositoryService) DeleteRepository(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteRepository(ctx, name, intent.ownership())
}
