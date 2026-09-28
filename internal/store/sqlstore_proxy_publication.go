package store

import (
	"context"
	"errors"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// ErrProxyResultSuperseded means a later-started fetch already published a
// result for this cache identity. The caller may finish its own response, but
// must not make its result authoritative in the shared cache.
var ErrProxyResultSuperseded = errors.New("proxy result superseded")

// BeginProxyFetchByRepositoryID obtains a database-ordered generation before
// upstream I/O. The epoch prevents an expired, recycled key from accepting an
// old in-flight result. Expired state is pruned on subsequent cache misses, so
// failed requests to distinct paths do not retain permanent generation rows.
func (s *SQLStore) BeginProxyFetchByRepositoryID(ctx context.Context, repositoryID, path string, leaseUntil time.Time) (domain.ProxyFetchToken, error) {
	const pruneBatch = 256
	now := time.Now()
	// Cold fetches retire a bounded batch of expired generations. The outer
	// predicate protects a row renewed after the subquery selected it.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM proxy_cache_state
		WHERE expires_at_ns <= ? AND (repository_id, path) IN
		(SELECT repository_id, path FROM proxy_cache_state
		 WHERE expires_at_ns <= ? ORDER BY expires_at_ns LIMIT ?)`,
		now.UnixNano(), now.UnixNano(), pruneBatch); err != nil {
		return domain.ProxyFetchToken{}, err
	}
	cutoff := formatTime(now)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM negative_cache
		WHERE expires_at < ? AND (repository_id, path) IN
		(SELECT repository_id, path FROM negative_cache
		 WHERE expires_at < ? ORDER BY expires_at LIMIT ?)`,
		cutoff, cutoff, pruneBatch); err != nil {
		return domain.ProxyFetchToken{}, err
	}
	const query = `INSERT INTO proxy_cache_state
		(repository_id, path, epoch, next_sequence, published_sequence, expires_at_ns)
		VALUES (?, ?, ?, 1, 0, ?)
		ON CONFLICT(repository_id, path) DO UPDATE SET
			epoch = CASE WHEN proxy_cache_state.expires_at_ns <= ?
				THEN excluded.epoch ELSE proxy_cache_state.epoch END,
			next_sequence = CASE WHEN proxy_cache_state.expires_at_ns <= ?
				THEN 1 ELSE proxy_cache_state.next_sequence + 1 END,
			published_sequence = CASE WHEN proxy_cache_state.expires_at_ns <= ?
				THEN 0 ELSE proxy_cache_state.published_sequence END,
			expires_at_ns = CASE WHEN proxy_cache_state.expires_at_ns <= ?
				THEN excluded.expires_at_ns
				WHEN proxy_cache_state.expires_at_ns > excluded.expires_at_ns
				THEN proxy_cache_state.expires_at_ns ELSE excluded.expires_at_ns END
		RETURNING epoch, next_sequence`
	var token domain.ProxyFetchToken
	// The bounded global prune may not reach this path. Reset an expired row
	// inside the atomic UPSERT so renewing it never revives its former epoch.
	nowNS := now.UnixNano()
	err := s.db.QueryRowContext(ctx, query,
		repositoryID, path, newRepositoryID(), leaseUntil.UnixNano(),
		nowNS, nowNS, nowNS, nowNS,
	).Scan(&token.Epoch, &token.Sequence)
	if isRepositoryReferenceError(err) {
		return domain.ProxyFetchToken{}, domain.ErrNotFound
	}
	return token, err
}

// claimProxyResultTx advances authority only for a still-live generation that
// follows the last published result. The UPDATE row lock serializes concurrent
// 200, 304, and 404 publications across PostgreSQL replicas and SQLite writers.
func claimProxyResultTx(ctx context.Context, tx *dialectTx, repositoryID, path string, token domain.ProxyFetchToken) (bool, error) {
	if token.Epoch == "" || token.Sequence <= 0 {
		return false, ErrProxyResultSuperseded
	}
	result, err := tx.ExecContext(ctx, `UPDATE proxy_cache_state SET published_sequence = ?
		WHERE repository_id = ? AND path = ? AND epoch = ?
		AND published_sequence < ? AND expires_at_ns > ?`,
		token.Sequence, repositoryID, path, token.Epoch, token.Sequence, time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *SQLStore) PublishProxyNotFoundByRepositoryID(ctx context.Context, repositoryID, path string, token domain.ProxyFetchToken, expiresAt time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	claimed, err := claimProxyResultTx(ctx, tx, repositoryID, path, token)
	if err != nil || !claimed {
		return false, err
	}
	// A 404 supersedes a positive row, including an immutable one. Retaining
	// the row would resurrect old bytes when the short negative TTL expires.
	// Only this cache identity is removed; content-addressed blobs and any
	// independent digest aliases remain available to their own paths.
	if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE repository_id = ? AND path = ?`, repositoryID, path); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO negative_cache (repository_id, path, expires_at)
		VALUES (?, ?, ?) ON CONFLICT(repository_id, path) DO UPDATE SET expires_at = excluded.expires_at`,
		repositoryID, path, formatTime(expiresAt))
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *SQLStore) PublishProxyNotModifiedByRepositoryID(ctx context.Context, repositoryID, path string, id int64, token domain.ProxyFetchToken, validatedAt time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	claimed, err := claimProxyResultTx(ctx, tx, repositoryID, path, token)
	if err != nil || !claimed {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assets SET validated_at = ? WHERE repository_id = ? AND path = ? AND id = ?`,
		formatTime(validatedAt), repositoryID, path, id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err // rollback the claim if the original asset disappeared
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM negative_cache WHERE repository_id = ? AND path = ?`, repositoryID, path); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
