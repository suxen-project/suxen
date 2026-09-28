package content

import (
	"context"
	"errors"
	"io"

	"github.com/suxen-project/suxen/internal/domain"
)

// OpenStoredAsset opens the bytes for a resolved asset. A migration may move
// its blob after the caller reads the asset row, so a missing blob prompts a
// fresh lookup by immutable repository and asset IDs. Only a placement change
// for the same digest may redirect the read; a deleted or replaced asset must
// not turn into a read of another generation at the same path.
func (rt *Runtime) OpenStoredAsset(
	ctx context.Context,
	asset domain.Asset,
) (io.ReadCloser, domain.BlobInfo, error) {
	const maxPlacements = 3
	current := asset
	for attempt := 0; attempt < maxPlacements; attempt++ {
		blobStore, err := rt.BlobStores.Store(ctx, current.BlobStore)
		if err == nil {
			var reader io.ReadCloser
			var info domain.BlobInfo
			reader, info, err = blobStore.Get(ctx, current.Digest)
			if err == nil {
				return reader, info, nil
			}
		}
		if !errors.Is(err, domain.ErrNotFound) || attempt == maxPlacements-1 {
			return nil, domain.BlobInfo{}, err
		}
		fresh, refreshErr := rt.metaFor(domain.Repository{
			Name: asset.Repository, ID: asset.RepositoryID,
		}).AssetByID(ctx, asset.ID)
		if refreshErr != nil {
			return nil, domain.BlobInfo{}, refreshErr
		}
		if fresh.Digest != asset.Digest || fresh.BlobStore == current.BlobStore {
			return nil, domain.BlobInfo{}, err
		}
		current = fresh
	}
	return nil, domain.BlobInfo{}, domain.ErrNotFound
}
