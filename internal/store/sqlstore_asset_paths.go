package store

import (
	"context"
	"errors"
)

// AssetPathsByRepositoryID reads a bounded path-keyset page for one repository.
// The last returned path is the cursor for the next page; it remains valid if
// that asset is deleted between queries.
func (s *SQLStore) AssetPathsByRepositoryID(ctx context.Context, repositoryID, prefix, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 200 {
		return nil, errors.New("invalid asset path page limit")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM assets WHERE repository_id = ? AND path >= ? AND path LIKE ? ESCAPE '!'
         AND substr(path, 1, length(?)) = ? AND path > ? ORDER BY path LIMIT ?`,
		repositoryID, prefix, escapeLikePrefix(prefix)+"%", prefix, prefix, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := make([]string, 0, limit)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}
