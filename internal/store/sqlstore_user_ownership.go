package store

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Ownership expresses the declarative-provisioning effect of a control-plane
// mutation, resolved inside the mutation's own transaction so the account and
// its ownership record can never diverge across a crash or a replica boundary.
type Ownership struct {
	// Declarative marks a provisioning write. The ownership record is adopted or
	// refreshed with Fingerprint on a save, and removed on a delete.
	Declarative bool
	// Fingerprint is the salted secret digest persisted on a declarative save.
	Fingerprint string
	// Force lets an imperative caller take a resource away from provisioning.
	// Without it, an imperative mutation of a managed resource is rejected.
	Force bool
}

// UserSave is an atomic account-plus-roles mutation and its ownership effect.
// Password empty keeps the existing hash on update; a nil Roles slice keeps the
// existing assignments on update. PreserveAdmin leaves the current admin value
// untouched on update. Create inserts a new account.
type UserSave struct {
	Username      string
	Password      string
	Admin         bool
	PreserveAdmin bool
	Roles         []string
	Create        bool
	Ownership     Ownership
}

// SaveUser commits an account, its role assignments, and its ownership record in
// one transaction serialized on ("user", name). An imperative mutation of a
// provisioning-managed account returns domain.ErrManaged unless Force is set.
func (s *SQLStore) SaveUser(ctx context.Context, save UserSave) error {
	if !domain.ValidUsername(save.Username) {
		return domain.ErrInvalidUsername
	}
	if save.Create && save.Password == "" {
		return domain.ErrPasswordRequired
	}
	// Hash outside the transaction so password hashing never holds the write lock.
	var encodedPassword string
	if save.Password != "" {
		hashed, err := hashPassword(save.Password)
		if err != nil {
			return err
		}
		encodedPassword = hashed
	}
	var identity string
	if save.Create {
		var err error
		identity, err = newUserIdentity()
		if err != nil {
			return err
		}
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()

	// The managed-ownership guard applies to updates: a create either takes a
	// free name or fails the unique constraint as a conflict.
	managed, err := s.prepareOwnershipTx(ctx, transaction, "user", save.Username, save.Ownership, !save.Create)
	if err != nil {
		return err
	}

	if save.Create {
		_, err = transaction.ExecContext(ctx,
			`INSERT INTO users (username, password_hash, admin, created_at, identity) VALUES (?, ?, ?, ?, ?)`,
			save.Username, encodedPassword, save.Admin, formatTime(time.Now()), identity,
		)
		if isUniqueConstraint(err) {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
	} else {
		var existingPasswordHash string
		err = transaction.QueryRowContext(ctx,
			`SELECT password_hash FROM users WHERE username = ?`, save.Username,
		).Scan(&existingPasswordHash)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		passwordChanged := save.Password != "" && !verifyPassword(existingPasswordHash, save.Password)
		var result sql.Result
		switch {
		case passwordChanged && save.PreserveAdmin:
			result, err = transaction.ExecContext(ctx,
				`UPDATE users SET password_hash = ? WHERE username = ?`,
				encodedPassword, save.Username,
			)
		case passwordChanged:
			result, err = transaction.ExecContext(ctx,
				`UPDATE users SET password_hash = ?, admin = ? WHERE username = ?`,
				encodedPassword, save.Admin, save.Username,
			)
		case !save.PreserveAdmin:
			result, err = transaction.ExecContext(ctx,
				`UPDATE users SET admin = ? WHERE username = ?`, save.Admin, save.Username,
			)
		}
		if err != nil {
			return err
		}
		if result != nil {
			if err := requireAffectedRow(result); err != nil {
				return err
			}
		}
	}

	if save.Roles != nil || save.Create {
		roles := save.Roles
		if roles == nil {
			roles = []string{}
		}
		if err := setUserRolesTx(ctx, transaction, save.Username, roles); err != nil {
			return err
		}
	}

	if err := applyOwnershipTx(ctx, transaction, "user", save.Username, save.Ownership, managed, false); err != nil {
		return err
	}
	return transaction.Commit()
}

// ReplaceUserRoles replaces a user's direct role assignments and applies the
// ownership effect in one transaction. Account fields are left untouched.
func (s *SQLStore) ReplaceUserRoles(
	ctx context.Context,
	username string,
	roles []string,
	ownership Ownership,
) error {
	if !domain.ValidUsername(username) {
		return domain.ErrInvalidUsername
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()

	managed, err := s.prepareOwnershipTx(ctx, transaction, "user", username, ownership, true)
	if err != nil {
		return err
	}
	if roles == nil {
		roles = []string{}
	}
	if err := setUserRolesTx(ctx, transaction, username, roles); err != nil {
		return err
	}
	if err := applyOwnershipTx(ctx, transaction, "user", username, ownership, managed, false); err != nil {
		return err
	}
	return transaction.Commit()
}

// sqlExecer is satisfied by both *dialectDB and *dialectTx, so a row writer can
// run standalone (the plain primitives) or inside an ownership transaction (the
// Save/Delete commands) without duplicating its SQL.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// writeOwned runs mutate and the resource's ownership effect in one transaction
// serialized on (kind, name). guard rejects an imperative mutation of a managed
// resource without force; isDelete removes the ownership record instead of
// adopting it. It is the shared scaffold behind the per-kind Save/Delete
// commands so each kind supplies only its mutation SQL.
func (s *SQLStore) writeOwned(
	ctx context.Context,
	kind string,
	name string,
	ownership Ownership,
	guard bool,
	isDelete bool,
	mutate func(context.Context, *dialectTx) error,
) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	managed, err := s.prepareOwnershipTx(ctx, transaction, kind, name, ownership, guard)
	if err != nil {
		return err
	}
	// A declarative delete (prune) must not remove a resource whose ownership was
	// transferred away since the prune plan was captured: if the record is gone
	// the resource is now API-owned, so skip it under the same keyed lock rather
	// than delete on a stale plan.
	if isDelete && ownership.Declarative && !managed {
		return domain.ErrNotFound
	}
	mutateErr := mutate(ctx, transaction)
	// When a declarative delete finds the resource row already gone (for example a
	// cascade from deleting its parent) but the ownership record is still present,
	// clear the record in this transaction rather than orphan it: the resource is
	// gone, so ownership has nothing left to protect.
	if isDelete && ownership.Declarative && errors.Is(mutateErr, domain.ErrNotFound) {
		mutateErr = nil
	}
	if mutateErr != nil {
		return mutateErr
	}
	if err := applyOwnershipTx(ctx, transaction, kind, name, ownership, managed, isDelete); err != nil {
		return err
	}
	return transaction.Commit()
}

// prepareOwnershipTx serializes the resource key, reads whether the resource is
// currently provisioning-managed, and rejects an imperative mutation that would
// silently seize a managed resource without Force.
func (s *SQLStore) prepareOwnershipTx(
	ctx context.Context,
	transaction *dialectTx,
	kind string,
	name string,
	ownership Ownership,
	guard bool,
) (managed bool, err error) {
	if err := lockOwnershipTx(ctx, transaction, kind, name); err != nil {
		return false, err
	}
	managed, err = ownershipManagedTx(ctx, transaction, kind, name)
	if err != nil {
		return false, err
	}
	if guard && !ownership.Declarative && managed && !ownership.Force {
		return managed, domain.ErrManaged
	}
	return managed, nil
}

// applyOwnershipTx records the ownership effect of a committed mutation:
// declarative saves adopt or refresh the record; a delete removes it; an
// imperative forced transfer relinquishes provisioning ownership.
func applyOwnershipTx(
	ctx context.Context,
	transaction *dialectTx,
	kind string,
	name string,
	ownership Ownership,
	wasManaged bool,
	isDelete bool,
) error {
	switch {
	case isDelete:
		return deleteProvisionRecordTx(ctx, transaction, kind, name)
	case ownership.Declarative:
		return putProvisionRecordTx(ctx, transaction, ProvisionRecord{
			Kind:              kind,
			Name:              name,
			SecretFingerprint: ownership.Fingerprint,
			UpdatedAt:         time.Now().UTC(),
		})
	case ownership.Force && wasManaged:
		return deleteProvisionRecordTx(ctx, transaction, kind, name)
	default:
		return nil
	}
}

// lockOwnershipTx serializes ownership decisions for one resource key across
// replicas. On PostgreSQL a transaction advisory lock covers the key even when
// the record row is absent, which a row FOR UPDATE cannot. On SQLite the store
// opens every transaction as an immediate write, which already serializes.
func lockOwnershipTx(ctx context.Context, transaction *dialectTx, kind, name string) error {
	if transaction.dialect != dialectPostgres {
		return nil
	}
	_, err := transaction.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(?)`, ownershipLockKey(kind, name),
	)
	return err
}

func ownershipLockKey(kind, name string) int64 {
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(name))
	return int64(digest.Sum64())
}

func ownershipManagedTx(ctx context.Context, transaction *dialectTx, kind, name string) (bool, error) {
	query := `SELECT 1 FROM provision_records WHERE kind = ? AND name = ?`
	if transaction.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	var present int
	err := transaction.QueryRowContext(ctx, query, kind, name).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func putProvisionRecordTx(ctx context.Context, transaction *dialectTx, record ProvisionRecord) error {
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO provision_records (
			kind, name, secret_fingerprint, updated_at
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, name) DO UPDATE SET
			secret_fingerprint = excluded.secret_fingerprint,
			updated_at = excluded.updated_at`
	_, err := transaction.ExecContext(ctx, query,
		record.Kind, record.Name, record.SecretFingerprint, formatTime(record.UpdatedAt),
	)
	return err
}

func deleteProvisionRecordTx(ctx context.Context, transaction *dialectTx, kind, name string) error {
	_, err := transaction.ExecContext(ctx,
		`DELETE FROM provision_records WHERE kind = ? AND name = ?`, kind, name,
	)
	return err
}
