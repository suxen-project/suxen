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

// SetTrustPolicy creates or replaces a repository signature policy.
func (s *SQLStore) SetTrustPolicy(
	ctx context.Context,
	policy domain.TrustPolicy,
) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := writeTrustPolicyRow(ctx, transaction, transaction.dialect, policy); err != nil {
		return err
	}
	return transaction.Commit()
}

// TrustPolicySave is an atomic trust-policy mutation and its ownership effect.
type TrustPolicySave struct {
	Policy    domain.TrustPolicy
	Ownership Ownership
}

// SaveTrustPolicy commits a repository signature policy and its ownership record
// in one transaction serialized on ("trustPolicy", repository). The repository
// row lock the mutation takes stays inside that transaction.
func (s *SQLStore) SaveTrustPolicy(ctx context.Context, save TrustPolicySave) error {
	policy := save.Policy
	// A trust policy is upserted, so ownership is always guarded: managed status
	// derives from the provisioning record, not from whether the row exists yet.
	return s.writeOwned(ctx, "trustPolicy", policy.Repository, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			return writeTrustPolicyRow(ctx, transaction, transaction.dialect, policy)
		})
}

// writeTrustPolicyRow validates and upserts one repository signature policy.
// Serializing on the repository row (FOR UPDATE on PostgreSQL) keeps conversion
// to a group from leaving an accepted policy that downloads would never enforce.
func writeTrustPolicyRow(
	ctx context.Context,
	executor sqlExecer,
	dialect string,
	policy domain.TrustPolicy,
) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	query := `SELECT type, format FROM repositories WHERE name = ?`
	if dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	var repository domain.Repository
	if err := executor.QueryRowContext(ctx, query, policy.Repository).Scan(
		&repository.Type, &repository.Format,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if repository.Type == "group" {
		return domain.ErrInvalidTrustPolicy
	}
	if policy.Mode == "verify-on-push" &&
		(repository.Type != "hosted" || !formatSupportsSignedPush(repository.Format)) {
		return domain.ErrInvalidTrustPolicy
	}
	policy.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode trust policy: %w", err)
	}
	const upsert = `
		INSERT INTO trust_policies (repository_id, policy, updated_at)
		VALUES ((SELECT id FROM repositories WHERE name = ?), ?, ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			policy = excluded.policy,
			updated_at = excluded.updated_at`
	_, err = executor.ExecContext(
		ctx,
		upsert,
		policy.Repository,
		string(encoded),
		formatTime(time.Now().UTC()),
	)
	return err
}

// TrustPolicy returns a repository signature policy.
func (s *SQLStore) TrustPolicy(
	ctx context.Context,
	repositoryName string,
) (domain.TrustPolicy, error) {
	const query = `
		SELECT policy, updated_at
		FROM trust_policies
		WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`
	var policy domain.TrustPolicy
	var encoded string
	var updatedAt string
	err := s.db.QueryRowContext(ctx, query, repositoryName).Scan(
		&encoded,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, domain.ErrNotFound
	}
	if err != nil {
		return policy, err
	}
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return policy, fmt.Errorf("decode trust policy: %w", err)
	}
	policy.UpdatedAt, err = parseTime(updatedAt)
	return policy, err
}

// DeleteTrustPolicy removes a repository signature policy and its ownership
// record in one transaction.
func (s *SQLStore) DeleteTrustPolicy(
	ctx context.Context,
	repositoryName string,
	ownership Ownership,
) error {
	return s.writeOwned(ctx, "trustPolicy", repositoryName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(
				ctx,
				`DELETE FROM trust_policies WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`,
				repositoryName,
			)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

// EffectiveTrustPolicy resolves the policy that applies to a repository: its own
// when it has one, otherwise the instance-wide default. A repository policy
// overrides the default entirely; there is no partial merge. ErrNotFound means
// neither layer defines a policy.
func (s *SQLStore) EffectiveTrustPolicy(
	ctx context.Context,
	repositoryName string,
) (domain.TrustPolicy, error) {
	policy, err := s.TrustPolicy(ctx, repositoryName)
	if err == nil {
		return policy, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return policy, err
	}
	return s.TrustPolicyDefaults(ctx)
}

// SetTrustPolicyDefaults creates or replaces the instance-wide trust-policy
// default that repositories without their own policy inherit.
func (s *SQLStore) SetTrustPolicyDefaults(
	ctx context.Context,
	policy domain.TrustPolicy,
) error {
	return writeTrustPolicyDefaultsRow(ctx, s.db, policy)
}

// TrustPolicyDefaultsSave is an atomic trust-policy-default mutation and its
// ownership effect.
type TrustPolicyDefaultsSave struct {
	Policy    domain.TrustPolicy
	Ownership Ownership
}

// SaveTrustPolicyDefaults commits the instance-wide trust-policy default and its
// ownership record in one transaction serialized on ("trustPolicy", default).
func (s *SQLStore) SaveTrustPolicyDefaults(ctx context.Context, save TrustPolicyDefaultsSave) error {
	return s.writeOwned(ctx, "trustPolicy", domain.InstanceDefaultsName, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			return writeTrustPolicyDefaultsRow(ctx, transaction, save.Policy)
		})
}

func writeTrustPolicyDefaultsRow(ctx context.Context, executor sqlExecer, policy domain.TrustPolicy) error {
	if err := policy.ValidateDefaults(); err != nil {
		return err
	}
	// A global push policy would also apply to proxies and native
	// publish protocols that cannot carry Suxen signature headers. Require
	// verify-on-push to be selected explicitly on a compatible hosted repo.
	if policy.Mode == "verify-on-push" {
		return domain.ErrInvalidTrustPolicy
	}
	// Ownership is derived from the provisioning record, so the persisted
	// document never carries managed; zero it before encoding.
	policy.UpdatedAt = time.Time{}
	policy.Managed = false
	encoded, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode trust policy: %w", err)
	}
	const query = `
		INSERT INTO trust_policy_defaults (singleton, policy, updated_at)
		VALUES (1, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			policy = excluded.policy,
			updated_at = excluded.updated_at`
	_, err = executor.ExecContext(
		ctx,
		query,
		string(encoded),
		formatTime(time.Now().UTC()),
	)
	return err
}

func formatSupportsSignedPush(format string) bool {
	switch format {
	case "cargo", "npm", "pypi":
		return false
	default:
		return true
	}
}

// TrustPolicyDefaults returns the instance-wide trust-policy default.
func (s *SQLStore) TrustPolicyDefaults(
	ctx context.Context,
) (domain.TrustPolicy, error) {
	const query = `
		SELECT policy, updated_at
		FROM trust_policy_defaults
		WHERE singleton = 1`
	var policy domain.TrustPolicy
	var encoded string
	var updatedAt string
	err := s.db.QueryRowContext(ctx, query).Scan(&encoded, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, domain.ErrNotFound
	}
	if err != nil {
		return policy, err
	}
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return policy, fmt.Errorf("decode trust policy: %w", err)
	}
	policy.UpdatedAt, err = parseTime(updatedAt)
	return policy, err
}

// DeleteTrustPolicyDefaults removes the instance-wide trust-policy default and
// its ownership record in one transaction.
func (s *SQLStore) DeleteTrustPolicyDefaults(ctx context.Context, ownership Ownership) error {
	return s.writeOwned(ctx, "trustPolicy", domain.InstanceDefaultsName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(
				ctx,
				`DELETE FROM trust_policy_defaults WHERE singleton = 1`,
			)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}
