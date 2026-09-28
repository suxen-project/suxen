package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/store"
)

// AccountStore is the backend capability the account service consumes: the
// atomic account-plus-roles mutations that fold the ownership record into one
// transaction serialized on the account key. The SQL store satisfies it.
type AccountStore interface {
	SaveUser(context.Context, store.UserSave) error
	ReplaceUserRoles(context.Context, string, []string, store.Ownership) error
	DeleteUser(context.Context, string, store.Ownership) error
}

// SaveUserCommand creates or updates a local account together with its role
// assignments. Password empty keeps the existing hash on update; a nil Roles
// slice keeps the existing assignments on update. PreserveAdmin leaves the
// current admin value untouched on update when the API omits that field.
type SaveUserCommand struct {
	Username      string
	Password      string
	Admin         bool
	PreserveAdmin bool
	Roles         []string
	Create        bool
	Intent        Intent
}

// AccountService applies account mutations and their ownership effects
// atomically. It is stateless beyond its backend and safe to share.
type AccountService struct {
	backend AccountStore
}

// NewAccountService composes the account command service over its backend.
func NewAccountService(backend AccountStore) *AccountService {
	return &AccountService{backend: backend}
}

// SaveUser applies an account-plus-roles mutation and its ownership effect
// atomically. It returns domain.ErrManaged when an imperative caller targets a
// provisioning-managed account without Force.
func (s *AccountService) SaveUser(ctx context.Context, cmd SaveUserCommand) error {
	return s.backend.SaveUser(ctx, store.UserSave{
		Username:      cmd.Username,
		Password:      cmd.Password,
		Admin:         cmd.Admin,
		PreserveAdmin: cmd.PreserveAdmin,
		Roles:         cmd.Roles,
		Create:        cmd.Create,
		Ownership:     cmd.Intent.ownership(),
	})
}

// SetUserRoles replaces a user's direct roles and applies the ownership effect
// atomically, leaving account fields untouched.
func (s *AccountService) SetUserRoles(ctx context.Context, username string, roles []string, intent Intent) error {
	return s.backend.ReplaceUserRoles(ctx, username, roles, intent.ownership())
}

// DeleteUser removes an account and its ownership record atomically.
func (s *AccountService) DeleteUser(ctx context.Context, username string, intent Intent) error {
	return s.backend.DeleteUser(ctx, username, intent.ownership())
}
