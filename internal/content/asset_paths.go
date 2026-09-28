package content

import (
	"context"

	"github.com/suxen-project/suxen/internal/store"
)

const formatAssetPathBatchSize = 128

// visitAssetPaths keeps the format SPI's enumeration memory bounded to one
// path-only page. The cursor is the path string, so deletion of a visited row
// does not invalidate later pages.
func visitAssetPaths(ctx context.Context, view store.RepositoryView, prefix string, visit func(string) (bool, error)) error {
	var after string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		paths, err := view.AssetPaths(ctx, prefix, after, formatAssetPathBatchSize)
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := visit(path)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) < formatAssetPathBatchSize {
			return nil
		}
		after = paths[len(paths)-1]
	}
}
