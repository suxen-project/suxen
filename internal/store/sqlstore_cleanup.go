package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/predicate"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

const cleanupPolicyColumns = `
	name,
	repositories,
	criteria,
	keep_last,
	retention_order,
	action,
	enabled,
	created_at,
	updated_at`

const taskColumns = `
	id,
	type,
	status,
	policy,
	repository,
	dry_run,
	result,
	error,
	created_at,
	started_at,
	completed_at`

// Classification returns the ordered classification rules for a repository.
func (s *SQLStore) Classification(
	ctx context.Context,
	repositoryName string,
) (domain.ClassificationConfig, error) {
	return classificationFrom(ctx, s.db, repositoryName)
}

// classificationReader is the subset of query methods shared by *dialectDB and
// *dialectTx, so classification and repository reads run either standalone or
// inside the relabel transaction, which must resolve its targets from committed
// state while holding the relabel lock rather than from a pre-transaction read.
type classificationReader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func classificationFrom(
	ctx context.Context,
	reader classificationReader,
	repositoryName string,
) (domain.ClassificationConfig, error) {
	var config domain.ClassificationConfig
	var rulesJSON string
	var updatedAt string
	const query = `
		SELECT (SELECT name FROM repositories WHERE id = classification_rules.repository_id),
			rules, inherit_global, updated_at
		FROM classification_rules
		WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`
	err := reader.QueryRowContext(ctx, query, repositoryName).Scan(
		&config.Repository,
		&rulesJSON,
		&config.InheritGlobal,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if _, repositoryErr := repositoryFrom(ctx, reader, repositoryName); repositoryErr != nil {
			return config, repositoryErr
		}
		return domain.ClassificationConfig{
			Repository:    repositoryName,
			Rules:         []domain.ClassificationRule{},
			InheritGlobal: true,
		}, nil
	}
	if err != nil {
		return config, err
	}
	if err := decodeJSONNumbers(rulesJSON, &config.Rules); err != nil {
		return config, fmt.Errorf("decode classification rules: %w", err)
	}
	config.UpdatedAt, err = parseTime(updatedAt)
	return config, err
}

// effectiveClassification prepends the instance-wide default's rules to a
// repository config's when it inherits, so a repository rule overrides a global
// rule that assigns the same key.
func (s *SQLStore) effectiveClassification(
	ctx context.Context,
	config domain.ClassificationConfig,
) (domain.ClassificationConfig, error) {
	return effectiveClassificationFrom(ctx, s.db, config)
}

func effectiveClassificationFrom(
	ctx context.Context,
	reader classificationReader,
	config domain.ClassificationConfig,
) (domain.ClassificationConfig, error) {
	if !config.InheritGlobal {
		return config, nil
	}
	global, err := classificationDefaultsFrom(ctx, reader)
	if errors.Is(err, domain.ErrNotFound) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	merged := config
	merged.Rules = append(
		append([]domain.ClassificationRule{}, global.Rules...),
		config.Rules...,
	)
	return merged, nil
}

// SetClassification replaces a repository's rules and rewrites classification.*
// on every existing asset from the effective (default-merged) rules, in one
// transaction.
func (s *SQLStore) SetClassification(
	ctx context.Context,
	config domain.ClassificationConfig,
) (int, error) {
	if err := config.Validate(); err != nil {
		return 0, err
	}
	if config.Rules == nil {
		config.Rules = []domain.ClassificationRule{}
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	if err := lockClassificationRelabel(ctx, transaction); err != nil {
		return 0, err
	}
	repository, err := repositoryFrom(ctx, transaction, config.Repository)
	if err != nil {
		return 0, err
	}
	effective, err := effectiveClassificationFrom(ctx, transaction, config)
	if err != nil {
		return 0, err
	}
	count, err := writeClassificationTx(ctx, transaction, config, repository, effective.Rules, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// classificationRelabelLock names the advisory lock shared by publications and
// held exclusively by every classification relabel. Default and per-repository
// relabels are exclusive with each other, so a default write's target set cannot
// be invalidated by a concurrent per-repository write. Relabel commands acquire
// it after writeOwned takes the per-key ownership lock; publications acquire only
// the shared relabel lock before reading rules or touching asset rows.
const classificationRelabelLock = "classification-relabel"

func lockClassificationRelabel(ctx context.Context, transaction *dialectTx) error {
	return lockOwnershipTx(ctx, transaction, classificationRelabelLock, "")
}

func lockClassificationPublication(ctx context.Context, transaction *dialectTx) error {
	if transaction.dialect != dialectPostgres {
		// SQLite's immediate write transactions already serialize both paths.
		return nil
	}
	_, err := transaction.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock_shared(?)`, ownershipLockKey(classificationRelabelLock, ""),
	)
	return err
}

// ClassificationSave is an atomic classification mutation (rules plus the asset
// relabel) and its ownership effect.
type ClassificationSave struct {
	Config    domain.ClassificationConfig
	Ownership Ownership
}

// SaveClassification commits a repository's classification rules, the asset
// relabel, and its ownership record in one transaction serialized on
// ("classification", repository).
func (s *SQLStore) SaveClassification(ctx context.Context, save ClassificationSave) error {
	config := save.Config
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Rules == nil {
		config.Rules = []domain.ClassificationRule{}
	}
	return s.writeOwned(ctx, "classification", config.Repository, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := lockClassificationRelabel(ctx, transaction); err != nil {
				return err
			}
			repository, err := repositoryFrom(ctx, transaction, config.Repository)
			if err != nil {
				return err
			}
			effective, err := effectiveClassificationFrom(ctx, transaction, config)
			if err != nil {
				return err
			}
			_, err = writeClassificationTx(ctx, transaction, config, repository, effective.Rules, time.Now().UTC())
			return err
		})
}

// writeClassificationTx upserts a repository's rules and rewrites classification.*
// on its assets from the effective rules, within the transaction.
func writeClassificationTx(
	ctx context.Context,
	transaction *dialectTx,
	config domain.ClassificationConfig,
	repository domain.Repository,
	effectiveRules []domain.ClassificationRule,
	now time.Time,
) (int, error) {
	rulesJSON, err := json.Marshal(config.Rules)
	if err != nil {
		return 0, fmt.Errorf("encode classification rules: %w", err)
	}
	const upsert = `
		INSERT INTO classification_rules (repository_id, rules, inherit_global, updated_at)
		VALUES ((SELECT id FROM repositories WHERE name = ?), ?, ?, ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			rules = excluded.rules,
			inherit_global = excluded.inherit_global,
			updated_at = excluded.updated_at`
	if _, err := transaction.ExecContext(
		ctx,
		upsert,
		config.Repository,
		string(rulesJSON),
		config.InheritGlobal,
		formatTime(now),
	); err != nil {
		return 0, err
	}
	return relabelRepositoryAssets(ctx, transaction, repository, effectiveRules, now)
}

// DeleteClassification removes a repository's own rules. The repository reverts
// to inheriting the instance-wide default, so its assets are relabeled from that
// default's rules (cleared when no default exists), in one transaction.
func (s *SQLStore) DeleteClassification(ctx context.Context, repositoryName string, ownership Ownership) error {
	return s.writeOwned(ctx, "classification", repositoryName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := lockClassificationRelabel(ctx, transaction); err != nil {
				return err
			}
			// Resolve the relabel inputs under the relabel lock. A missing repository
			// (for example a cascade delete of its parent) means there are no assets to
			// relabel, but a declarative prune must still clear the ownership record; an
			// imperative caller keeps the original 404 since the API guards repository
			// existence first.
			repository, repoErr := repositoryFrom(ctx, transaction, repositoryName)
			var inheritedRules []domain.ClassificationRule
			switch {
			case repoErr == nil:
				inherited, err := effectiveClassificationFrom(
					ctx,
					transaction,
					domain.ClassificationConfig{Repository: repositoryName, InheritGlobal: true},
				)
				if err != nil {
					return err
				}
				inheritedRules = inherited.Rules
			case !errors.Is(repoErr, domain.ErrNotFound):
				return repoErr
			case !ownership.Declarative:
				return repoErr
			}
			if _, err := transaction.ExecContext(
				ctx,
				`DELETE FROM classification_rules WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`,
				repositoryName,
			); err != nil {
				return err
			}
			if repoErr != nil {
				return nil
			}
			_, err := relabelRepositoryAssets(ctx, transaction, repository, inheritedRules, time.Now().UTC())
			return err
		})
}

// SetClassificationDefaults replaces the instance-wide classification default and
// relabels every inheriting repository's assets, all in one transaction.
func (s *SQLStore) SetClassificationDefaults(
	ctx context.Context,
	config domain.ClassificationConfig,
) (int, error) {
	if err := config.ValidateDefaults(); err != nil {
		return 0, err
	}
	if config.Rules == nil {
		config.Rules = []domain.ClassificationRule{}
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	if err := lockClassificationRelabel(ctx, transaction); err != nil {
		return 0, err
	}
	targets, err := inheritingRepositoryRulesFrom(ctx, transaction, config.Rules)
	if err != nil {
		return 0, err
	}
	total, err := writeClassificationDefaultsTx(ctx, transaction, config, targets, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// ClassificationDefaultsSave is an atomic classification-default mutation (rules
// plus relabel of inheriting repositories) and its ownership effect.
type ClassificationDefaultsSave struct {
	Config    domain.ClassificationConfig
	Ownership Ownership
}

// SaveClassificationDefaults commits the instance-wide default, the relabel of
// every inheriting repository, and its ownership record in one transaction
// serialized on ("classification", default).
func (s *SQLStore) SaveClassificationDefaults(ctx context.Context, save ClassificationDefaultsSave) error {
	config := save.Config
	if err := config.ValidateDefaults(); err != nil {
		return err
	}
	if config.Rules == nil {
		config.Rules = []domain.ClassificationRule{}
	}
	return s.writeOwned(ctx, "classification", domain.InstanceDefaultsName, save.Ownership, true, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := lockClassificationRelabel(ctx, transaction); err != nil {
				return err
			}
			targets, err := inheritingRepositoryRulesFrom(ctx, transaction, config.Rules)
			if err != nil {
				return err
			}
			_, err = writeClassificationDefaultsTx(ctx, transaction, config, targets, time.Now().UTC())
			return err
		})
}

// writeClassificationDefaultsTx upserts the instance-wide default rules and
// relabels every inheriting repository, within the transaction.
func writeClassificationDefaultsTx(
	ctx context.Context,
	transaction *dialectTx,
	config domain.ClassificationConfig,
	targets []repositoryRules,
	now time.Time,
) (int, error) {
	rulesJSON, err := json.Marshal(config.Rules)
	if err != nil {
		return 0, fmt.Errorf("encode classification rules: %w", err)
	}
	const upsert = `
		INSERT INTO classification_defaults (singleton, rules, updated_at)
		VALUES (1, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			rules = excluded.rules,
			updated_at = excluded.updated_at`
	if _, err := transaction.ExecContext(
		ctx,
		upsert,
		string(rulesJSON),
		formatTime(now),
	); err != nil {
		return 0, err
	}
	return relabelRepositoryTargets(ctx, transaction, targets, now)
}

// DeleteClassificationDefaults removes the instance-wide default and relabels
// every inheriting repository from its own rules alone, in one transaction.
func (s *SQLStore) DeleteClassificationDefaults(ctx context.Context, ownership Ownership) error {
	return s.writeOwned(ctx, "classification", domain.InstanceDefaultsName, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := lockClassificationRelabel(ctx, transaction); err != nil {
				return err
			}
			targets, err := inheritingRepositoryRulesFrom(ctx, transaction, nil)
			if err != nil {
				return err
			}
			result, err := transaction.ExecContext(
				ctx,
				`DELETE FROM classification_defaults WHERE singleton = 1`,
			)
			if err != nil {
				return err
			}
			if err := requireAffectedRow(result); err != nil {
				return err
			}
			_, err = relabelRepositoryTargets(ctx, transaction, targets, time.Now().UTC())
			return err
		})
}

// ClassificationDefaults returns the instance-wide classification default.
func (s *SQLStore) ClassificationDefaults(
	ctx context.Context,
) (domain.ClassificationConfig, error) {
	return classificationDefaultsFrom(ctx, s.db)
}

func classificationDefaultsFrom(
	ctx context.Context,
	reader classificationReader,
) (domain.ClassificationConfig, error) {
	var config domain.ClassificationConfig
	var rulesJSON string
	var updatedAt string
	const query = `
		SELECT rules, updated_at
		FROM classification_defaults
		WHERE singleton = 1`
	err := reader.QueryRowContext(ctx, query).Scan(
		&rulesJSON,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return config, domain.ErrNotFound
	}
	if err != nil {
		return config, err
	}
	if err := decodeJSONNumbers(rulesJSON, &config.Rules); err != nil {
		return config, fmt.Errorf("decode classification rules: %w", err)
	}
	config.UpdatedAt, err = parseTime(updatedAt)
	return config, err
}

// repositoryRules pairs a repository with the effective classification rules to
// apply to its assets.
type repositoryRules struct {
	repository domain.Repository
	rules      []domain.ClassificationRule
}

// inheritingRepositoryRules resolves, for every repository that inherits the
// instance-wide default, its effective rules (globalRules ahead of the
// repository's own). The relabel commands call it inside their transaction while
// holding the relabel lock so the target set reflects committed state and cannot
// be invalidated by a concurrent per-repository classification write.
func (s *SQLStore) inheritingRepositoryRules(
	ctx context.Context,
	globalRules []domain.ClassificationRule,
) ([]repositoryRules, error) {
	return inheritingRepositoryRulesFrom(ctx, s.db, globalRules)
}

func inheritingRepositoryRulesFrom(
	ctx context.Context,
	reader classificationReader,
	globalRules []domain.ClassificationRule,
) ([]repositoryRules, error) {
	repositories, err := repositoriesFrom(ctx, reader)
	if err != nil {
		return nil, err
	}
	targets := make([]repositoryRules, 0, len(repositories))
	for _, repository := range repositories {
		repoConfig, err := classificationFrom(ctx, reader, repository.Name)
		if err != nil {
			return nil, err
		}
		if !repoConfig.InheritGlobal {
			continue
		}
		effective := append(
			append([]domain.ClassificationRule{}, globalRules...),
			repoConfig.Rules...,
		)
		targets = append(targets, repositoryRules{repository: repository, rules: effective})
	}
	return targets, nil
}

// relabelRepositoryTargets applies each resolved target's rules within the
// transaction, returning the total number of assets rewritten.
func relabelRepositoryTargets(
	ctx context.Context,
	transaction *dialectTx,
	targets []repositoryRules,
	now time.Time,
) (int, error) {
	total := 0
	for _, target := range targets {
		count, err := relabelRepositoryAssets(ctx, transaction, target.repository, target.rules, now)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// relabelRepositoryTx refreshes a repository's derived asset columns
// (component, version, and retention group) and reapplies its effective rules
// after a change to the repository fields that they and classification
// predicates project. The caller holds the relabel lock.
func relabelRepositoryTx(ctx context.Context, transaction *dialectTx, repositoryName string) error {
	repository, err := repositoryFrom(ctx, transaction, repositoryName)
	if err != nil {
		return err
	}
	if err := refreshDerivedColumnsTx(ctx, transaction, repository); err != nil {
		return err
	}
	config, err := classificationFrom(ctx, transaction, repositoryName)
	if err != nil {
		return err
	}
	effective, err := effectiveClassificationFrom(ctx, transaction, config)
	if err != nil || len(effective.Rules) == 0 {
		return err
	}
	_, err = relabelRepositoryAssets(ctx, transaction, repository, effective.Rules, time.Now().UTC())
	return err
}

// relabelRepositoryAssets rewrites classification.* on every asset of one
// repository from the given effective rules, within the transaction.
func relabelRepositoryAssets(
	ctx context.Context,
	transaction *dialectTx,
	repository domain.Repository,
	rules []domain.ClassificationRule,
	now time.Time,
) (int, error) {
	assets, err := scanRepositoryAssets(ctx, transaction, repository.Name)
	if err != nil {
		return 0, err
	}
	effective := domain.ClassificationConfig{Rules: rules}
	for _, asset := range assets {
		labels := classificationLabels(effective, asset, repository, now)
		attributes := assetattrs.SetClassificationLabels(asset.Attributes, labels)
		attributesJSON, err := json.Marshal(attributes)
		if err != nil {
			return 0, fmt.Errorf("encode asset attributes: %w", err)
		}
		if _, err := transaction.ExecContext(
			ctx,
			`UPDATE assets SET attributes = ? WHERE id = ?`,
			string(attributesJSON),
			asset.ID,
		); err != nil {
			return 0, err
		}
	}
	return len(assets), nil
}

// classificationLabels evaluates the ordered rules against one asset's projected
// attributes. Every matching rule contributes classification.<Key> = Value; a
// later rule overwrites an earlier one on the same key. No matching rule yields
// no labels, so the asset carries no classification.* attributes.
func classificationLabels(
	config domain.ClassificationConfig,
	asset domain.Asset,
	repository domain.Repository,
	now time.Time,
) map[string]string {
	if len(config.Rules) == 0 {
		return nil
	}
	// Derived labels from an earlier evaluation are not source attributes for
	// this one. Otherwise predicates on classification.* can toggle their own
	// outcome each time unchanged rules are applied.
	attributes := assetattrs.Project(asset, repository)
	delete(attributes, assetattrs.ClassificationNamespace)
	labels := make(map[string]string)
	for _, rule := range config.Rules {
		if predicate.MatchAll(attributes, rule.When, now) {
			labels[rule.Key] = rule.Value
		}
	}
	return labels
}

// scanRepositoryAssets reads the same projection as the public asset API. On
// PostgreSQL, lock rows before reading their attributes so an annotation or
// verification committed while relabel waits is included in the decision and
// cannot be overwritten by the later classification write. The relabel lock is
// acquired first by the caller; publication follows the same lock order.
func scanRepositoryAssets(
	ctx context.Context,
	tx *dialectTx,
	repositoryName string,
) ([]domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets
		WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)
		ORDER BY id`
	if tx.dialect == dialectPostgres {
		query += ` FOR UPDATE OF assets`
	}
	rows, err := tx.QueryContext(
		ctx,
		query,
		repositoryName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := make([]domain.Asset, 0)
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

// CreateCleanupPolicy inserts a reusable cleanup policy.
func (s *SQLStore) CreateCleanupPolicy(ctx context.Context, policy domain.CleanupPolicy) error {
	normalizeCleanupPolicy(&policy)
	if err := validateCleanupPolicyRepositories(ctx, s, policy); err != nil {
		return err
	}
	repositories, criteria, err := encodeCleanupPolicy(policy)
	if err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
		return err
	}
	if err := validateRepositoryFilterTx(ctx, transaction, policy.Repositories); err != nil {
		return err
	}
	if err := insertCleanupPolicyRow(ctx, transaction, policy, repositories, criteria); err != nil {
		return err
	}
	return transaction.Commit()
}

// CleanupPolicySave is an atomic cleanup-policy mutation and its ownership effect.
type CleanupPolicySave struct {
	Policy    domain.CleanupPolicy
	Create    bool
	Ownership Ownership
}

// SaveCleanupPolicy commits a cleanup policy and its ownership record in one
// transaction serialized on ("cleanupPolicy", name).
func (s *SQLStore) SaveCleanupPolicy(ctx context.Context, save CleanupPolicySave) error {
	policy := save.Policy
	normalizeCleanupPolicy(&policy)
	if err := validateCleanupPolicyRepositories(ctx, s, policy); err != nil {
		return err
	}
	repositories, criteria, err := encodeCleanupPolicy(policy)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, "cleanupPolicy", policy.Name, save.Ownership, !save.Create, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
				return err
			}
			if err := validateRepositoryFilterTx(ctx, transaction, policy.Repositories); err != nil {
				return err
			}
			if save.Create {
				return insertCleanupPolicyRow(ctx, transaction, policy, repositories, criteria)
			}
			return updateCleanupPolicyRow(ctx, transaction, policy, repositories, criteria)
		})
}

// DeleteCleanupPolicy removes a cleanup policy and its ownership record in one
// transaction.
func (s *SQLStore) DeleteCleanupPolicy(ctx context.Context, name string, ownership Ownership) error {
	return s.writeOwned(ctx, "cleanupPolicy", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(ctx, `DELETE FROM cleanup_policies WHERE name = ?`, name)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

func insertCleanupPolicyRow(
	ctx context.Context,
	executor sqlExecer,
	policy domain.CleanupPolicy,
	repositories string,
	criteria string,
) error {
	const query = `
		INSERT INTO cleanup_policies (
			name, repositories, criteria, keep_last, retention_order, action,
			enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := executor.ExecContext(
		ctx,
		query,
		policy.Name,
		repositories,
		criteria,
		policy.KeepLast,
		policy.Order,
		policy.Action,
		policy.Enabled,
		formatTime(policy.CreatedAt),
		formatTime(policy.UpdatedAt),
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func updateCleanupPolicyRow(
	ctx context.Context,
	executor sqlExecer,
	policy domain.CleanupPolicy,
	repositories string,
	criteria string,
) error {
	const query = `
		UPDATE cleanup_policies
		SET repositories = ?, criteria = ?, keep_last = ?, retention_order = ?,
			action = ?, enabled = ?, updated_at = ?
		WHERE name = ?`
	result, err := executor.ExecContext(
		ctx,
		query,
		repositories,
		criteria,
		policy.KeepLast,
		policy.Order,
		policy.Action,
		policy.Enabled,
		formatTime(time.Now().UTC()),
		policy.Name,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

// CleanupPolicy returns one cleanup policy by name.
func (s *SQLStore) CleanupPolicy(
	ctx context.Context,
	name string,
) (domain.CleanupPolicy, error) {
	query := `SELECT ` + cleanupPolicyColumns + ` FROM cleanup_policies WHERE name = ?`
	return scanCleanupPolicy(s.db.QueryRowContext(ctx, query, name))
}

// CleanupPolicies returns cleanup policies ordered by name.
func (s *SQLStore) CleanupPolicies(ctx context.Context) ([]domain.CleanupPolicy, error) {
	query := `SELECT ` + cleanupPolicyColumns + ` FROM cleanup_policies ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]domain.CleanupPolicy, 0)
	for rows.Next() {
		policy, err := scanCleanupPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func encodeCleanupPolicy(policy domain.CleanupPolicy) (string, string, error) {
	repositories, err := json.Marshal(uniqueStrings(policy.Repositories))
	if err != nil {
		return "", "", fmt.Errorf("encode cleanup repositories: %w", err)
	}
	criteria, err := json.Marshal(policy.Criteria)
	if err != nil {
		return "", "", fmt.Errorf("encode cleanup criteria: %w", err)
	}
	return string(repositories), string(criteria), nil
}

func scanCleanupPolicy(source scanner) (domain.CleanupPolicy, error) {
	var policy domain.CleanupPolicy
	var repositories string
	var criteria string
	var createdAt string
	var updatedAt string
	err := source.Scan(
		&policy.Name,
		&repositories,
		&criteria,
		&policy.KeepLast,
		&policy.Order,
		&policy.Action,
		&policy.Enabled,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, domain.ErrNotFound
	}
	if err != nil {
		return policy, err
	}
	if err := json.Unmarshal([]byte(repositories), &policy.Repositories); err != nil {
		return policy, fmt.Errorf("decode cleanup repositories: %w", err)
	}
	if err := decodeJSONNumbers(criteria, &policy.Criteria); err != nil {
		return policy, fmt.Errorf("decode cleanup criteria: %w", err)
	}
	policy.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return policy, err
	}
	policy.UpdatedAt, err = parseTime(updatedAt)
	return policy, err
}

func normalizeCleanupPolicy(policy *domain.CleanupPolicy) {
	if policy.Action == "" {
		policy.Action = "delete"
	}
	if policy.Order == "" {
		policy.Order = domain.CleanupOrderUpdatedAt
	}
	now := time.Now().UTC()
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = now
	}
	if policy.UpdatedAt.IsZero() {
		policy.UpdatedAt = now
	}
}

func validateCleanupPolicyRepositories(
	ctx context.Context,
	metadata *SQLStore,
	policy domain.CleanupPolicy,
) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	for _, repositoryName := range uniqueStrings(policy.Repositories) {
		if _, err := metadata.Repository(ctx, repositoryName); err != nil {
			return fmt.Errorf("resolve cleanup repository %q: %w", repositoryName, err)
		}
	}
	return nil
}

// CreateTask records a queued administrative operation.
func (s *SQLStore) CreateTask(ctx context.Context, task domain.Task) (domain.Task, error) {
	if task.Status == "" {
		task.Status = "queued"
	}
	if task.Result == nil {
		task.Result = make(map[string]any)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	resultJSON, err := json.Marshal(task.Result)
	if err != nil {
		return task, fmt.Errorf("encode task result: %w", err)
	}
	const query = `
		INSERT INTO tasks (
			type, status, policy, repository, dry_run, result, error,
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`
	err = s.db.QueryRowContext(
		ctx,
		query,
		task.Type,
		task.Status,
		task.Policy,
		task.Repository,
		task.DryRun,
		string(resultJSON),
		task.Error,
		formatTime(task.CreatedAt),
		formatOptionalTime(task.StartedAt),
		formatOptionalTime(task.CompletedAt),
	).Scan(&task.ID)
	if err != nil {
		return task, err
	}
	return task, nil
}

// UpdateTask replaces the mutable state of an administrative operation.
func (s *SQLStore) UpdateTask(ctx context.Context, task domain.Task) error {
	resultJSON, err := json.Marshal(task.Result)
	if err != nil {
		return fmt.Errorf("encode task result: %w", err)
	}
	const query = `
		UPDATE tasks
		SET status = ?, result = ?, error = ?, started_at = ?, completed_at = ?
		WHERE id = ?`
	result, err := s.db.ExecContext(
		ctx,
		query,
		task.Status,
		string(resultJSON),
		task.Error,
		formatOptionalTime(task.StartedAt),
		formatOptionalTime(task.CompletedAt),
		task.ID,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

// Task returns one administrative operation by identifier.
func (s *SQLStore) Task(ctx context.Context, id int64) (domain.Task, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE id = ?`
	return scanTask(s.db.QueryRowContext(ctx, query, id))
}

// Tasks returns recent administrative operations in reverse creation order.
func (s *SQLStore) Tasks(ctx context.Context, limit int) ([]domain.Task, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT ` + taskColumns + ` FROM tasks ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]domain.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// TaskPage returns a bounded page ordered by descending task ID. A zero
// snapshot starts a traversal; subsequent calls reuse the returned SnapshotID
// and pass the last item ID as beforeID.
func (s *SQLStore) TaskPage(
	ctx context.Context,
	snapshotID int64,
	beforeID int64,
	limit int,
) (IDPage[domain.Task], error) {
	page := IDPage[domain.Task]{}
	if snapshotID < 0 || beforeID < 0 || limit < 1 || limit > 1000 {
		return page, fmt.Errorf("invalid task page")
	}
	if snapshotID == 0 {
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT COALESCE(MAX(id), 0) FROM tasks`,
		).Scan(&snapshotID); err != nil {
			return page, err
		}
	}
	page.SnapshotID = snapshotID
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE id <= ?`
	arguments := []any{snapshotID}
	if beforeID > 0 {
		query += ` AND id < ?`
		arguments = append(arguments, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	items := make([]domain.Task, 0, limit+1)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return page, err
		}
		items = append(items, task)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(items) > limit {
		page.HasMore = true
		items = items[:limit]
	}
	page.Items = items
	return page, nil
}

func scanTask(source scanner) (domain.Task, error) {
	var task domain.Task
	var resultJSON string
	var createdAt string
	var startedAt sql.NullString
	var completedAt sql.NullString
	err := source.Scan(
		&task.ID,
		&task.Type,
		&task.Status,
		&task.Policy,
		&task.Repository,
		&task.DryRun,
		&resultJSON,
		&task.Error,
		&createdAt,
		&startedAt,
		&completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return task, domain.ErrNotFound
	}
	if err != nil {
		return task, err
	}
	if err := decodeJSONNumbers(resultJSON, &task.Result); err != nil {
		return task, fmt.Errorf("decode task result: %w", err)
	}
	task.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return task, err
	}
	if task.StartedAt, err = parseOptionalTime(startedAt); err != nil {
		return task, err
	}
	task.CompletedAt, err = parseOptionalTime(completedAt)
	return task, err
}

// AcquireLease atomically claims or renews a named scheduler lease.
func (s *SQLStore) AcquireLease(
	ctx context.Context,
	name string,
	holder string,
	now time.Time,
	expiresAt time.Time,
) (bool, error) {
	const query = `
		INSERT INTO leader_leases (name, holder, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			holder = excluded.holder,
			expires_at = excluded.expires_at
		WHERE leader_leases.expires_at <= ? OR leader_leases.holder = excluded.holder`
	result, err := s.db.ExecContext(
		ctx,
		query,
		name,
		holder,
		formatTime(expiresAt),
		formatTime(now),
	)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

// Lease returns the current owner and expiry of a named scheduler lease.
func (s *SQLStore) Lease(ctx context.Context, name string) (domain.Lease, error) {
	var lease domain.Lease
	var expiresAt string
	const query = `SELECT name, holder, expires_at FROM leader_leases WHERE name = ?`
	err := s.db.QueryRowContext(ctx, query, name).Scan(
		&lease.Name,
		&lease.Holder,
		&expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return lease, domain.ErrNotFound
	}
	if err != nil {
		return lease, err
	}
	lease.ExpiresAt, err = parseTime(expiresAt)
	return lease, err
}

func (s *SQLStore) ReleaseLease(ctx context.Context, name string, holder string) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM leader_leases WHERE name = ? AND holder = ?`,
		name,
		holder,
	)
	return err
}

func formatOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// derivedColumnsRevision versions how the host computes the derived asset
// columns. Bump it whenever an already stored asset's component, version,
// version key, or retention group would change, so Migrate recomputes them.
const derivedColumnsRevision = "1"

// derivedRevision is the revision a repository's derived columns must carry:
// the host's, plus the format plugin's grouping revision when it declares one.
func derivedRevision(format string) string {
	registered, found := spiformat.Lookup(format)
	if !found {
		return derivedColumnsRevision
	}
	if versioned, ok := registered.(spiformat.RetentionGroupingRevision); ok {
		return derivedColumnsRevision + "/" + versioned.RetentionGroupingRevision()
	}
	return derivedColumnsRevision
}

// refreshDerivedColumnsTx recomputes the derived columns of one repository's
// assets and records the revision they were computed with.
func refreshDerivedColumnsTx(ctx context.Context, transaction *dialectTx, repository domain.Repository) error {
	query := `SELECT id, path, format_path, digest, size, content_type, kind, reference,
		updated_at, validated_at, component, component_version, component_version_key, retention_group
		FROM assets WHERE repository_id = ? ORDER BY id`
	if transaction.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	rows, err := transaction.QueryContext(ctx, query, repository.ID)
	if err != nil {
		return err
	}
	type change struct {
		id      int64
		derived derivedColumns
	}
	var changes []change
	for rows.Next() {
		asset := domain.Asset{Repository: repository.Name}
		var updatedAt string
		var validatedAt sql.NullString
		var stored derivedColumns
		if err := rows.Scan(&asset.ID, &asset.Path, &asset.FormatPath, &asset.Digest, &asset.Size,
			&asset.ContentType, &asset.Kind, &asset.Reference, &updatedAt, &validatedAt,
			&stored.component, &stored.version, &stored.versionKey, &stored.group); err != nil {
			rows.Close()
			return err
		}
		if asset.UpdatedAt, err = parseTime(updatedAt); err != nil {
			rows.Close()
			return err
		}
		asset.ValidatedAt = asset.UpdatedAt
		if validatedAt.Valid {
			if asset.ValidatedAt, err = parseTime(validatedAt.String); err != nil {
				rows.Close()
				return err
			}
		}
		if want := derivedAssetColumns(asset, repository); want != stored {
			changes = append(changes, change{id: asset.ID, derived: want})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, update := range changes {
		if _, err := transaction.ExecContext(ctx,
			`UPDATE assets SET component = ?, component_version = ?, component_version_key = ?, retention_group = ? WHERE id = ?`,
			update.derived.component, update.derived.version, update.derived.versionKey, update.derived.group, update.id,
		); err != nil {
			return err
		}
	}
	_, err = transaction.ExecContext(ctx,
		`UPDATE repositories SET derived_revision = ? WHERE id = ?`, derivedRevision(repository.Format), repository.ID)
	return err
}

// reconcileDerivedColumns recomputes the derived columns of every repository
// whose stored revision differs from the running one: after the migration
// that added them, and after an upgrade that changes how the host or a format
// plugin groups assets. Raw patterns are RE2 expressions and retention groups
// may come from plugins, neither of which SQL can evaluate, so it runs in Go.
func reconcileDerivedColumns(ctx context.Context, transaction *dialectTx) error {
	rows, err := transaction.QueryContext(ctx,
		`SELECT name, format, derived_revision FROM repositories ORDER BY id`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var name, format, revision string
		if err := rows.Scan(&name, &format, &revision); err != nil {
			rows.Close()
			return err
		}
		if revision != derivedRevision(format) {
			stale = append(stale, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(stale) == 0 {
		return err
	}
	// Publications derive the columns under the shared lock; exclude them
	// while rows are rewritten.
	if err := lockClassificationRelabel(ctx, transaction); err != nil {
		return err
	}
	// Raw attributes come from the recomputed columns, which classification
	// rules may match, so Raw repositories relabel as a format config change
	// does. Other formats' attributes are unchanged.
	for _, name := range stale {
		repository, err := repositoryFrom(ctx, transaction, name)
		if err == nil {
			if repository.Format == "raw" {
				err = relabelRepositoryTx(ctx, transaction, name)
			} else {
				err = refreshDerivedColumnsTx(ctx, transaction, repository)
			}
		}
		if err != nil {
			return fmt.Errorf("recompute derived columns of repository %q: %w", name, err)
		}
	}
	return nil
}
