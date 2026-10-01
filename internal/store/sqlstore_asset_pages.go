package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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

// ComponentPage is one keyset page of distinct component names with every
// asset row of those components.
type ComponentPage struct {
	Components []string
	Assets     []domain.Asset
	HasMore    bool
}

// componentAfterAsset resolves a keyset cursor: the component of the asset a
// previous page ended on. A removed asset reports domain.ErrNotFound.
func (s *SQLStore) componentAfterAsset(ctx context.Context, repositoryID string, afterID int64) (string, error) {
	if afterID == 0 {
		return "", nil
	}
	var component string
	err := s.db.QueryRowContext(ctx,
		`SELECT component FROM assets WHERE repository_id = ? AND id = ? AND component <> ''`,
		repositoryID, afterID,
	).Scan(&component)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	return component, err
}

// ComponentPageByRepositoryID pages distinct non-empty component names after
// the component of asset afterID through the component index, then reads only
// those components' rows, so a page never scans unrelated assets.
func (s *SQLStore) ComponentPageByRepositoryID(ctx context.Context, repositoryID string, afterID int64, limit int) (ComponentPage, error) {
	page := ComponentPage{Components: make([]string, 0), Assets: make([]domain.Asset, 0)}
	if limit < 1 || limit > 200 || afterID < 0 {
		return page, errors.New("invalid component page request")
	}
	after, err := s.componentAfterAsset(ctx, repositoryID, afterID)
	if err != nil {
		return page, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT component FROM assets WHERE repository_id = ? AND component > ?
		 ORDER BY component LIMIT ?`,
		repositoryID, after, limit+1,
	)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var component string
		if err := rows.Scan(&component); err != nil {
			rows.Close()
			return page, err
		}
		if len(page.Components) == limit {
			page.HasMore = true
			break
		}
		page.Components = append(page.Components, component)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(page.Components) == 0 {
		return page, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(page.Components)), ", ")
	arguments := []any{repositoryID}
	for _, component := range page.Components {
		arguments = append(arguments, component)
	}
	assetRows, err := s.db.QueryContext(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND component IN (`+placeholders+`)
		 ORDER BY component, component_version, id`,
		arguments...,
	)
	if err != nil {
		return page, err
	}
	defer assetRows.Close()
	for assetRows.Next() {
		asset, err := scanAsset(assetRows)
		if err != nil {
			return page, err
		}
		asset.RepositoryID = repositoryID
		page.Assets = append(page.Assets, asset)
	}
	return page, assetRows.Err()
}
