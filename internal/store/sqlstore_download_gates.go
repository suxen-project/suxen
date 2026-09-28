package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// SetDownloadGate creates or replaces a repository's attribute-based read gate.
func (s *SQLStore) SetDownloadGate(
	ctx context.Context,
	gate domain.DownloadGate,
) error {
	if err := gate.Validate(); err != nil {
		return err
	}
	if err := s.validateDownloadGateRepository(ctx, gate.Repository); err != nil {
		return err
	}
	return writeDownloadGateRow(ctx, s.db, gate)
}

// DownloadGateSave is an atomic download-gate mutation and its ownership effect.
type DownloadGateSave struct {
	Gate      domain.DownloadGate
	Ownership Ownership
}

// SaveDownloadGate commits a repository read gate and its ownership record in one
// transaction serialized on ("downloadGate", repository).
func (s *SQLStore) SaveDownloadGate(ctx context.Context, save DownloadGateSave) error {
	gate := save.Gate
	if err := gate.Validate(); err != nil {
		return err
	}
	if err := s.validateDownloadGateRepository(ctx, gate.Repository); err != nil {
		return err
	}
	return s.writeOwned(ctx, "downloadGate", gate.Repository, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			return writeDownloadGateRow(ctx, transaction, gate)
		})
}

func (s *SQLStore) validateDownloadGateRepository(ctx context.Context, name string) error {
	repository, err := s.Repository(ctx, name)
	if err != nil {
		return err
	}
	if repository.Type == "group" {
		return domain.ErrInvalidDownloadGate
	}
	return nil
}

func writeDownloadGateRow(ctx context.Context, executor sqlExecer, gate domain.DownloadGate) error {
	criteria, err := json.Marshal(gate.Criteria)
	if err != nil {
		return fmt.Errorf("encode download gate criteria: %w", err)
	}
	const query = `
		INSERT INTO download_gates (
			repository_id, criteria, enabled, inherit_global, updated_at
		) VALUES ((SELECT id FROM repositories WHERE name = ?), ?, ?, ?, ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			criteria = excluded.criteria,
			enabled = excluded.enabled,
			inherit_global = excluded.inherit_global,
			updated_at = excluded.updated_at`
	_, err = executor.ExecContext(
		ctx,
		query,
		gate.Repository,
		string(criteria),
		gate.Enabled,
		gate.InheritGlobal,
		formatTime(time.Now().UTC()),
	)
	return err
}

// DownloadGate returns a repository's read gate.
func (s *SQLStore) DownloadGate(
	ctx context.Context,
	repositoryName string,
) (domain.DownloadGate, error) {
	const query = `
		SELECT (SELECT name FROM repositories WHERE id = download_gates.repository_id),
			criteria, enabled, inherit_global, updated_at
		FROM download_gates
		WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`
	var gate domain.DownloadGate
	var criteria string
	var updatedAt string
	err := s.db.QueryRowContext(ctx, query, repositoryName).Scan(
		&gate.Repository,
		&criteria,
		&gate.Enabled,
		&gate.InheritGlobal,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return gate, domain.ErrNotFound
	}
	if err != nil {
		return gate, err
	}
	if err := decodeJSONNumbers(criteria, &gate.Criteria); err != nil {
		return gate, fmt.Errorf("decode download gate criteria: %w", err)
	}
	gate.UpdatedAt, err = parseTime(updatedAt)
	return gate, err
}

// DeleteDownloadGate removes a repository's read gate and its ownership record in
// one transaction.
func (s *SQLStore) DeleteDownloadGate(
	ctx context.Context,
	repositoryName string,
	ownership Ownership,
) error {
	return s.writeOwned(ctx, "downloadGate", repositoryName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(
				ctx,
				`DELETE FROM download_gates WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`,
				repositoryName,
			)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

// SetDownloadGateDefaults creates or replaces the instance-wide download-gate
// default that inheriting repositories AND-extend.
func (s *SQLStore) SetDownloadGateDefaults(
	ctx context.Context,
	gate domain.DownloadGate,
) error {
	if err := gate.ValidateDefaults(); err != nil {
		return err
	}
	return writeDownloadGateDefaultsRow(ctx, s.db, gate)
}

// DownloadGateDefaultsSave is an atomic download-gate-default mutation and its
// ownership effect.
type DownloadGateDefaultsSave struct {
	Gate      domain.DownloadGate
	Ownership Ownership
}

// SaveDownloadGateDefaults commits the instance-wide download-gate default and its
// ownership record in one transaction serialized on ("downloadGate", default).
func (s *SQLStore) SaveDownloadGateDefaults(ctx context.Context, save DownloadGateDefaultsSave) error {
	if err := save.Gate.ValidateDefaults(); err != nil {
		return err
	}
	return s.writeOwned(ctx, "downloadGate", domain.InstanceDefaultsName, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			return writeDownloadGateDefaultsRow(ctx, transaction, save.Gate)
		})
}

func writeDownloadGateDefaultsRow(ctx context.Context, executor sqlExecer, gate domain.DownloadGate) error {
	criteria, err := json.Marshal(gate.Criteria)
	if err != nil {
		return fmt.Errorf("encode download gate criteria: %w", err)
	}
	const query = `
		INSERT INTO download_gate_defaults (
			singleton, criteria, enabled, updated_at
		) VALUES (1, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			criteria = excluded.criteria,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`
	_, err = executor.ExecContext(
		ctx,
		query,
		string(criteria),
		gate.Enabled,
		formatTime(time.Now().UTC()),
	)
	return err
}

// DownloadGateDefaults returns the instance-wide download-gate default.
func (s *SQLStore) DownloadGateDefaults(
	ctx context.Context,
) (domain.DownloadGate, error) {
	const query = `
		SELECT criteria, enabled, updated_at
		FROM download_gate_defaults
		WHERE singleton = 1`
	var gate domain.DownloadGate
	var criteria string
	var updatedAt string
	err := s.db.QueryRowContext(ctx, query).Scan(
		&criteria,
		&gate.Enabled,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return gate, domain.ErrNotFound
	}
	if err != nil {
		return gate, err
	}
	if err := decodeJSONNumbers(criteria, &gate.Criteria); err != nil {
		return gate, fmt.Errorf("decode download gate criteria: %w", err)
	}
	gate.UpdatedAt, err = parseTime(updatedAt)
	return gate, err
}

// DeleteDownloadGateDefaults removes the instance-wide download-gate default and
// its ownership record in one transaction.
func (s *SQLStore) DeleteDownloadGateDefaults(ctx context.Context, ownership Ownership) error {
	return s.writeOwned(ctx, "downloadGate", domain.InstanceDefaultsName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(
				ctx,
				`DELETE FROM download_gate_defaults WHERE singleton = 1`,
			)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}
