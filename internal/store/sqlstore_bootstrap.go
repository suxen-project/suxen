package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// CreateBootstrapAdmin publishes credentials only after the account, role,
// and token can all commit. In particular, a crash or failed token insert
// cannot leave a random password inaccessible to the operator.
func (s *SQLStore) CreateBootstrapAdmin(ctx context.Context, username, password, token string) error {
	if !domain.ValidUsername(username) {
		return domain.ErrInvalidUsername
	}
	if password == "" {
		return domain.ErrPasswordRequired
	}
	if token == "" {
		return errors.New("bootstrap token is required")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	identity, err := newUserIdentity()
	if err != nil {
		return err
	}
	tokenHash := sha256.Sum256([]byte(token))
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, admin, created_at, identity) VALUES (?, ?, ?, ?, ?)`,
		username, passwordHash, true, now, identity,
	); err != nil {
		if isUniqueConstraint(err) {
			return domain.ErrConflict
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_roles (username, role) VALUES (?, ?)`, username, "administrator",
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tokens (username, name, token_hash, scopes, created_at) VALUES (?, ?, ?, ?, ?)`,
		username, "bootstrap", hex.EncodeToString(tokenHash[:]), "null", now,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE provision_records SET secret_fingerprint = '', updated_at = ? WHERE kind = ? AND name = ?`,
		now, "user", username,
	); err != nil {
		return err
	}
	return tx.Commit()
}
