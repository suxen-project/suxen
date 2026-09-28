package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// RoleStore is the backend capability the role service consumes: the atomic
// role mutations that fold the ownership record into one transaction serialized
// on the role key. The SQL store satisfies it.
type RoleStore interface {
	SaveRole(context.Context, store.RoleSave) error
	DeleteRole(context.Context, string, store.Ownership) error
}

// SaveRoleCommand creates or updates a flat privilege bundle. Create inserts a
// new role; an update replaces its description and privileges.
type SaveRoleCommand struct {
	Role   domain.Role
	Create bool
	Intent Intent
}

// RoleService applies role mutations and their ownership effects atomically. It
// is stateless beyond its backend and safe to share.
type RoleService struct {
	backend RoleStore
}

// NewRoleService composes the role command service over its backend.
func NewRoleService(backend RoleStore) *RoleService {
	return &RoleService{backend: backend}
}

// SaveRole applies a role mutation and its ownership effect atomically. It
// returns domain.ErrManaged when an imperative caller targets a
// provisioning-managed role without Force.
func (s *RoleService) SaveRole(ctx context.Context, cmd SaveRoleCommand) error {
	return s.backend.SaveRole(ctx, store.RoleSave{
		Role:      cmd.Role,
		Create:    cmd.Create,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteRole removes a role and its ownership record atomically. It returns
// domain.ErrRoleReferencedByProvider when an OIDC provider mapping still names
// the role.
func (s *RoleService) DeleteRole(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteRole(ctx, name, intent.ownership())
}
