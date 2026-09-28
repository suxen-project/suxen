package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func (s *SQLStore) RepositoryByID(ctx context.Context, repositoryID string) (domain.Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories WHERE id = ?`, repositoryID))
}

func (s *SQLStore) DownloadGateByRepositoryID(ctx context.Context, repositoryID string) (domain.DownloadGate, error) {
	const query = `SELECT (SELECT name FROM repositories WHERE id = download_gates.repository_id),
		criteria, enabled, inherit_global, updated_at FROM download_gates WHERE repository_id = ?`
	var gate domain.DownloadGate
	var criteria, updatedAt string
	err := s.db.QueryRowContext(ctx, query, repositoryID).Scan(&gate.Repository, &criteria, &gate.Enabled, &gate.InheritGlobal, &updatedAt)
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

func (s *SQLStore) EffectiveTrustPolicyByRepositoryID(ctx context.Context, repositoryID string) (domain.TrustPolicy, error) {
	const query = `SELECT policy, updated_at FROM trust_policies WHERE repository_id = ?`
	var policy domain.TrustPolicy
	var encoded, updatedAt string
	err := s.db.QueryRowContext(ctx, query, repositoryID).Scan(&encoded, &updatedAt)
	if err == nil {
		if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
			return policy, fmt.Errorf("decode trust policy: %w", err)
		}
		policy.UpdatedAt, err = parseTime(updatedAt)
		return policy, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return policy, err
	}
	// A stale identity has no repository row and must not inherit the global
	// policy as if it were a newly created repository with the same name.
	if _, err := s.RepositoryByID(ctx, repositoryID); err != nil {
		return policy, err
	}
	return s.TrustPolicyDefaults(ctx)
}

func (s *SQLStore) AssetByRepositoryID(ctx context.Context, repositoryID, path string) (domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = ? AND path = ?`
	asset, err := scanAsset(s.db.QueryRowContext(ctx, query, repositoryID, path))
	asset.RepositoryID = repositoryID
	return asset, err
}

func (s *SQLStore) AssetByPublicPath(ctx context.Context, repositoryID, path string, maxID int64) (domain.Asset, error) {
	// Separate probes keep both branches indexed: format_path has its own
	// repository index, while legacy hosted rows use the unique path index.
	lookup := func(condition string, args ...any) (domain.Asset, error) {
		query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = ? AND ` + condition
		values := append([]any{repositoryID}, args...)
		if maxID > 0 {
			query += ` AND id <= ?`
			values = append(values, maxID)
		}
		query += ` ORDER BY id DESC LIMIT 1`
		asset, err := scanAsset(s.db.QueryRowContext(ctx, query, values...))
		asset.RepositoryID = repositoryID
		return asset, err
	}
	formatted, formatErr := lookup(`format_path = ?`, path)
	if formatErr != nil && !errors.Is(formatErr, domain.ErrNotFound) {
		return domain.Asset{}, formatErr
	}
	plain, plainErr := lookup(`path = ? AND format_path = ''`, path)
	if plainErr != nil && !errors.Is(plainErr, domain.ErrNotFound) {
		return domain.Asset{}, plainErr
	}
	if formatErr == nil && (plainErr != nil || formatted.ID > plain.ID) {
		return formatted, nil
	}
	if plainErr == nil {
		return plain, nil
	}
	return domain.Asset{}, domain.ErrNotFound
}

func (s *SQLStore) AssetByIDAndRepositoryID(ctx context.Context, repositoryID string, assetID int64) (domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = ? AND id = ?`
	asset, err := scanAsset(s.db.QueryRowContext(ctx, query, repositoryID, assetID))
	asset.RepositoryID = repositoryID
	return asset, err
}

func (s *SQLStore) AssetsByRepositoryID(ctx context.Context, repositoryID, prefix string) ([]domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = ? AND path LIKE ? ESCAPE '!' ORDER BY path`
	rows, err := s.db.QueryContext(ctx, query, repositoryID, escapeLikePrefix(prefix)+"%")
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
		asset.RepositoryID = repositoryID
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (s *SQLStore) DeleteAssetByRepositoryID(ctx context.Context, repositoryID, repositoryName, path string) (domain.Asset, error) {
	query := `DELETE FROM assets WHERE repository_id = ? AND path = ? RETURNING ` + assetColumnsForReturning
	asset, err := scanAssetReturning(s.db.QueryRowContext(ctx, query, repositoryID, path), repositoryName)
	asset.RepositoryID = repositoryID
	return asset, err
}

// DeleteAssetWithCompanionsByRepositoryID deletes an artifact by path together
// with the companion metadata records its format declares, in one transaction, so
// an interactive delete cannot orphan a companion (and leak its blob) the way a
// separate follow-up delete could if it crashed in between. The artifact delete
// keeps delete-by-path semantics (a not-found path is ErrNotFound); companions
// are removed whether or not they were observed, restricted to the metadata kind
// so a non-metadata asset sharing a declared path is left alone.
func (s *SQLStore) DeleteAssetWithCompanionsByRepositoryID(ctx context.Context, repositoryID, repositoryName, path string, companionPaths []string) (domain.Asset, error) {
	if len(companionPaths) == 0 {
		return s.DeleteAssetByRepositoryID(ctx, repositoryID, repositoryName, path)
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Asset{}, err
	}
	defer transaction.Rollback()
	query := `DELETE FROM assets WHERE repository_id = ? AND path = ? RETURNING ` + assetColumnsForReturning
	asset, err := scanAssetReturning(transaction.QueryRowContext(ctx, query, repositoryID, path), repositoryName)
	if err != nil {
		return asset, err
	}
	asset.RepositoryID = repositoryID
	for _, companionPath := range companionPaths {
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM assets WHERE repository_id = ? AND path = ? AND kind = 'metadata'`,
			repositoryID, companionPath,
		); err != nil {
			return asset, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return asset, err
	}
	return asset, nil
}

// DeleteAssetByIDWithCompanionsByRepositoryID removes exactly the requested
// asset generation. Companion metadata is removed only after that row was
// deleted, within the same transaction.
func (s *SQLStore) DeleteAssetByIDWithCompanionsByRepositoryID(ctx context.Context, repositoryID, repositoryName string, assetID int64, companionPaths []string) (domain.Asset, error) {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Asset{}, err
	}
	defer transaction.Rollback()
	query := `DELETE FROM assets WHERE repository_id = ? AND id = ? RETURNING ` + assetColumnsForReturning
	asset, err := scanAssetReturning(transaction.QueryRowContext(ctx, query, repositoryID, assetID), repositoryName)
	if err != nil {
		return asset, err
	}
	asset.RepositoryID = repositoryID
	for _, companionPath := range companionPaths {
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM assets WHERE repository_id = ? AND path = ? AND kind = 'metadata'`,
			repositoryID, companionPath,
		); err != nil {
			return asset, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return asset, err
	}
	return asset, nil
}

func (s *SQLStore) DeleteOCIManifestByRepositoryID(ctx context.Context, repositoryID, repositoryName, manifestPath string) ([]domain.Asset, error) {
	marker := strings.LastIndex(manifestPath, "/manifests/")
	if marker < 0 {
		return nil, domain.ErrNotFound
	}
	imagePrefix := manifestPath[:marker+len("/manifests/")]
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	targetQuery := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = ? AND path = ?`
	if s.dialect == dialectPostgres {
		targetQuery += ` FOR UPDATE`
	}
	target, err := scanAsset(transaction.QueryRowContext(ctx, targetQuery, repositoryID, manifestPath))
	if err != nil {
		return nil, err
	}
	if target.Kind != "oci-manifest" || !strings.HasPrefix(target.Reference, "sha256:") {
		return nil, domain.ErrNotFound
	}
	rows, err := transaction.QueryContext(ctx,
		`DELETE FROM assets WHERE repository_id = ? AND kind = 'oci-manifest' AND digest = ? AND path LIKE ? ESCAPE '!' RETURNING `+assetColumnsForReturning,
		repositoryID, target.Digest, escapeLikePrefix(imagePrefix)+"%")
	if err != nil {
		return nil, err
	}
	deleted := make([]domain.Asset, 0)
	for rows.Next() {
		asset, scanErr := scanAssetReturning(rows, repositoryName)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		asset.RepositoryID = repositoryID
		deleted = append(deleted, asset)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		return nil, domain.ErrNotFound
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

func (s *SQLStore) NegativeCacheHitByRepositoryID(ctx context.Context, repositoryID, path string, now time.Time) (bool, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM negative_cache WHERE repository_id = ? AND path = ?`, repositoryID, path).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expiresAt, err := parseTime(encoded)
	if err != nil {
		return false, err
	}
	if !expiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM negative_cache
			WHERE repository_id = ? AND path = ? AND expires_at = ?`, repositoryID, path, encoded)
		return false, nil
	}
	return true, nil
}

func (s *SQLStore) ClearNegativeCacheByRepositoryID(ctx context.Context, repositoryID, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM negative_cache WHERE repository_id = ? AND path = ?`, repositoryID, path)
	return err
}

func (s *SQLStore) TouchAssetByRepositoryID(ctx context.Context, repositoryID string, assetID int64, at time.Time) error {
	if repositoryID == "" {
		return nil
	}
	return s.touchAsset(ctx, repositoryID, assetID, at)
}

func (s *SQLStore) RefreshAssetByRepositoryID(ctx context.Context, repositoryID string, assetID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assets SET validated_at = ? WHERE repository_id = ? AND id = ?`, formatTime(at), repositoryID, assetID)
	return err
}

func (s *SQLStore) SetAttributesByRepositoryID(ctx context.Context, repositoryID string, assetID int64, namespace string, value map[string]any) error {
	for attempt := 0; attempt < 8; attempt++ {
		var encoded string
		err := s.db.QueryRowContext(ctx, `SELECT attributes FROM assets WHERE repository_id = ? AND id = ?`, repositoryID, assetID).Scan(&encoded)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		attributes := make(map[string]any)
		if err := decodeJSONNumbers(encoded, &attributes); err != nil {
			return fmt.Errorf("decode asset attributes: %w", err)
		}
		attributes[namespace] = value
		updated, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("encode asset attributes: %w", err)
		}
		result, err := s.db.ExecContext(ctx,
			`UPDATE assets SET attributes = ? WHERE repository_id = ? AND id = ? AND attributes = ?`,
			string(updated), repositoryID, assetID, encoded)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 1 {
			return nil
		}
	}
	return domain.ErrConflict
}

func (s *SQLStore) DeleteAttributesByRepositoryID(ctx context.Context, repositoryID string, assetID int64, namespace string) error {
	for attempt := 0; attempt < 8; attempt++ {
		var encoded string
		err := s.db.QueryRowContext(ctx, `SELECT attributes FROM assets WHERE repository_id = ? AND id = ?`, repositoryID, assetID).Scan(&encoded)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		attributes := make(map[string]any)
		if err := decodeJSONNumbers(encoded, &attributes); err != nil {
			return fmt.Errorf("decode asset attributes: %w", err)
		}
		if _, found := attributes[namespace]; !found {
			return domain.ErrNotFound
		}
		delete(attributes, namespace)
		updated, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("encode asset attributes: %w", err)
		}
		result, err := s.db.ExecContext(ctx,
			`UPDATE assets SET attributes = ? WHERE repository_id = ? AND id = ? AND attributes = ?`,
			string(updated), repositoryID, assetID, encoded)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 1 {
			return nil
		}
	}
	return domain.ErrConflict
}
