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

// ComponentVersion identifies one stored component version in listing order:
// component ascending, then version key and version descending, so each
// component lists its highest versions first.
type ComponentVersion struct {
	Component, VersionKey, Version string
}

// ComponentVersionPage is one keyset page of component versions with every
// asset row of those versions.
type ComponentVersionPage struct {
	Versions []ComponentVersion
	Assets   []domain.Asset
	HasMore  bool
}

// afterComponentVersion is the keyset predicate for rows after cursor in
// listing order. The leading component bound lets both dialects start the
// index scan at the cursor's component.
func afterComponentVersion(cursor ComponentVersion) (string, []any) {
	return `component >= ? AND (component > ? OR (component = ? AND (component_version_key < ?
		OR (component_version_key = ? AND component_version < ?))))`,
		[]any{cursor.Component, cursor.Component, cursor.Component, cursor.VersionKey, cursor.VersionKey, cursor.Version}
}

// ComponentVersionPageByRepositoryID pages distinct stored component versions
// after the cursor (nil starts at the beginning) through the component
// version index, then reads only those versions' rows.
func (s *SQLStore) ComponentVersionPageByRepositoryID(
	ctx context.Context,
	repositoryID string,
	after *ComponentVersion,
	limit int,
) (ComponentVersionPage, error) {
	page := ComponentVersionPage{Versions: make([]ComponentVersion, 0), Assets: make([]domain.Asset, 0)}
	if limit < 1 || limit > 200 {
		return page, errors.New("invalid component version page request")
	}
	where := `repository_id = ? AND component <> ''`
	arguments := []any{repositoryID}
	if after != nil {
		predicate, values := afterComponentVersion(*after)
		where += ` AND ` + predicate
		arguments = append(arguments, values...)
	}
	const order = `component, component_version_key DESC, component_version DESC`
	rows, err := s.db.QueryContext(ctx,
		`SELECT component, component_version_key, component_version FROM assets WHERE `+where+`
		 GROUP BY component, component_version_key, component_version ORDER BY `+order+` LIMIT ?`,
		append(arguments, limit+1)...,
	)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var version ComponentVersion
		if err := rows.Scan(&version.Component, &version.VersionKey, &version.Version); err != nil {
			rows.Close()
			return page, err
		}
		if len(page.Versions) == limit {
			page.HasMore = true
			break
		}
		page.Versions = append(page.Versions, version)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(page.Versions) == 0 {
		return page, err
	}
	// The page's versions are contiguous in index order, so their rows are
	// those after the cursor and not after the page's last version.
	last := page.Versions[len(page.Versions)-1]
	beyond, values := afterComponentVersion(last)
	arguments = append(append(arguments, last.Component), values...)
	page.Assets, err = s.scanAssetRows(ctx, repositoryID,
		`SELECT `+assetColumns+` FROM assets WHERE `+where+` AND component <= ? AND NOT (`+beyond+`)
		 ORDER BY `+order+`, path`,
		arguments...,
	)
	return page, err
}

// AssetCountByRepositoryID counts a repository's asset rows.
func (s *SQLStore) AssetCountByRepositoryID(ctx context.Context, repositoryID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets WHERE repository_id = ?`, repositoryID).Scan(&count)
	return count, err
}

// RetentionGroupPageByRepositoryID pages the distinct stored retention groups
// after the given one through the retention-group index.
func (s *SQLStore) RetentionGroupPageByRepositoryID(ctx context.Context, repositoryID string, after string, limit int) ([]string, bool, error) {
	if limit < 1 {
		return nil, false, errors.New("invalid retention group page request")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT retention_group FROM assets WHERE repository_id = ? AND retention_group > ?
		 ORDER BY retention_group LIMIT ?`,
		repositoryID, after, limit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	groups := make([]string, 0, limit)
	more := false
	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			return nil, false, err
		}
		if len(groups) == limit {
			more = true
			break
		}
		groups = append(groups, group)
	}
	return groups, more, rows.Err()
}

// RetentionGroupAssetsByRepositoryID reads every asset of one stored group.
func (s *SQLStore) RetentionGroupAssetsByRepositoryID(ctx context.Context, repositoryID string, group string) ([]domain.Asset, error) {
	return s.scanAssetRows(ctx, repositoryID,
		`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND retention_group = ? ORDER BY id`,
		repositoryID, group)
}

// DirectoryAssetsByRepositoryID reads the direct children of a directory; an
// empty directory addresses the repository root.
func (s *SQLStore) DirectoryAssetsByRepositoryID(ctx context.Context, repositoryID string, directory string) ([]domain.Asset, error) {
	if directory == "" {
		return s.scanAssetRows(ctx, repositoryID,
			`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND path NOT LIKE '%/%' ORDER BY id`,
			repositoryID)
	}
	lower, upper := prefixRange(directory + "/")
	return s.scanAssetRows(ctx, repositoryID,
		`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND path >= ? AND path < ?
		 AND substr(path, length(?) + 1) NOT LIKE '%/%' ORDER BY id`,
		repositoryID, lower, upper, lower)
}

// SubtreeAssetsByRepositoryID reads every asset under a directory.
func (s *SQLStore) SubtreeAssetsByRepositoryID(ctx context.Context, repositoryID string, directory string) ([]domain.Asset, error) {
	lower, upper := prefixRange(directory + "/")
	return s.scanAssetRows(ctx, repositoryID,
		`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND path >= ? AND path < ? ORDER BY id`,
		repositoryID, lower, upper)
}

func (s *SQLStore) scanAssetRows(ctx context.Context, repositoryID string, query string, arguments ...any) ([]domain.Asset, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
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
