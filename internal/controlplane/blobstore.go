package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// BlobStoreStore is the narrow backend port the blob-store feature service needs
// for the metadata half of a blob-store mutation. Physical backend readiness and
// the in-memory backend cache stay with the caller: this service commits only the
// blob-store row and its ownership record. The SQL store satisfies it.
type BlobStoreStore interface {
	SaveBlobStore(context.Context, store.BlobStoreSave) error
	DeleteBlobStore(context.Context, string, store.Ownership) error
}

// SaveBlobStoreCommand creates or replaces a blob-store definition's editable
// metadata. Create inserts a new backend; otherwise the editable attributes are
// replaced in place. The caller verifies the physical backend before a create
// and remembers it after this returns.
type SaveBlobStoreCommand struct {
	BlobStore domain.BlobStore
	Create    bool
	Intent    Intent
}

// BlobStoreService applies the metadata half of a blob-store mutation and its
// ownership effect atomically. It is stateless beyond its store port and safe to
// construct per call site.
type BlobStoreService struct {
	backend BlobStoreStore
}

// NewBlobStoreService composes the service over the given store port.
func NewBlobStoreService(backend BlobStoreStore) *BlobStoreService {
	return &BlobStoreService{backend: backend}
}

// SaveBlobStore commits a blob-store row and its ownership record in one
// transaction.
func (s *BlobStoreService) SaveBlobStore(ctx context.Context, cmd SaveBlobStoreCommand) error {
	return s.backend.SaveBlobStore(ctx, store.BlobStoreSave{
		BlobStore: cmd.BlobStore,
		Create:    cmd.Create,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteBlobStore removes a blob-store row and its ownership record atomically.
func (s *BlobStoreService) DeleteBlobStore(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteBlobStore(ctx, name, intent.ownership())
}
