package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// blobStoreUsage reports physical occupancy of one blob store and how it splits
// between blobs still referenced by metadata and blobs a prune could reclaim.
type blobStoreUsage struct {
	BlobStore         string `json:"blobStore"`
	ObjectCount       int    `json:"objectCount"`
	TotalBytes        int64  `json:"totalBytes"`
	ReferencedCount   int    `json:"referencedCount"`
	ReferencedBytes   int64  `json:"referencedBytes"`
	UnreferencedCount int    `json:"unreferencedCount"`
	UnreferencedBytes int64  `json:"unreferencedBytes"`
}

// handleBlobStoreUsage reports physical usage for one blob store. It enumerates
// the backend, which the blob SPI documents as not cheap, so it is a deliberate
// on-demand action rather than part of the blob-store listing.
func (s *Server) handleBlobStoreUsage(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	usage, err := s.blobStoreUsage(r.Context(), name)
	httpx.WriteResult(w, usage, err)
}

func (s *Server) blobStoreUsage(ctx context.Context, name string) (*blobStoreUsage, error) {
	// Resolve the metadata record first so an unknown store returns 404 before
	// the expensive enumeration.
	if _, err := s.blobStoreReads().BlobStore(ctx, name); err != nil {
		return nil, err
	}
	referenced, err := s.blobReferenceGC().ReferencedDigests(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list referenced digests for blob store %q: %w", name, err)
	}
	configuredStore, err := s.blobStores.Store(ctx, name)
	if err != nil {
		return nil, err
	}
	usage := &blobStoreUsage{BlobStore: name}
	err = configuredStore.Walk(ctx, func(info domain.BlobInfo) error {
		usage.ObjectCount++
		usage.TotalBytes += info.Size
		if _, ok := referenced[info.Digest]; ok {
			usage.ReferencedCount++
			usage.ReferencedBytes += info.Size
		} else {
			usage.UnreferencedCount++
			usage.UnreferencedBytes += info.Size
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk blobs in %q: %w", name, err)
	}
	return usage, nil
}
