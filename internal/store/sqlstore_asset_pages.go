package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/suxen-project/suxen/internal/domain"
)

// AssetPageByRepositoryID reads a bounded page in path order. Cursor callers
// carry MaxID forward, excluding inserts and replacements made after page one.
// Existing rows can change and deletions can shorten the traversal.
func (s *SQLStore) AssetPageByRepositoryID(ctx context.Context, repositoryID string, request AssetPageRequest) (AssetPage, error) {
	page := AssetPage{Items: make([]domain.Asset, 0)}
	if request.Limit < 1 || request.Limit > 200 || request.AfterID < 0 || request.MaxID < 0 {
		return page, errors.New("invalid asset page request")
	}
	maxID := request.MaxID
	if maxID == 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM assets WHERE repository_id = ?`, repositoryID,
		).Scan(&maxID); err != nil {
			return page, err
		}
	}
	page.MaxID = maxID
	var afterPath string
	if request.AfterID > 0 {
		err := s.db.QueryRowContext(ctx,
			`SELECT path FROM assets WHERE repository_id = ? AND id = ? AND id <= ?`,
			repositoryID, request.AfterID, maxID,
		).Scan(&afterPath)
		if errors.Is(err, sql.ErrNoRows) {
			return page, domain.ErrNotFound
		}
		if err != nil {
			return page, err
		}
	}
	matchPath := "path"
	if request.PublicPrefix {
		matchPath = "COALESCE(NULLIF(format_path, ''), path)"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND id <= ? AND `+matchPath+` LIKE ? ESCAPE '!'
		 AND substr(`+matchPath+`, 1, length(?)) = ? AND path > ?
         ORDER BY path LIMIT ?`,
		repositoryID, maxID, escapeLikePrefix(request.Prefix)+"%", request.Prefix, request.Prefix,
		afterPath, request.Limit+1,
	)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return page, err
		}
		if len(page.Items) == request.Limit {
			page.HasMore = true
			break
		}
		asset.RepositoryID = repositoryID
		page.Items = append(page.Items, asset)
	}
	return page, rows.Err()
}

// MaxAssetID returns the global asset-identity high-water mark in one indexed lookup.
func (s *SQLStore) MaxAssetID(ctx context.Context) (int64, error) {
	var maximum int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM assets`).Scan(&maximum)
	return maximum, err
}
