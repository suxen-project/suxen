package server

import (
	"context"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

// collectBlobs is for assertions that need the complete set in memory.
func collectBlobs(ctx context.Context, store blob.Store) ([]domain.BlobInfo, error) {
	blobs := []domain.BlobInfo{}
	err := store.Walk(ctx, func(info domain.BlobInfo) error {
		blobs = append(blobs, info)
		return nil
	})
	return blobs, err
}
