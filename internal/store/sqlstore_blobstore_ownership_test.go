package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestBlobStoreOwnershipOnBothDialects exercises the blob-store ownership fold:
// the metadata half of a blob-store mutation and its provisioning-ownership
// record are committed in one transaction serialized on ("blobStore", name), so a
// declarative apply adopts the record, an imperative mutation of a managed store
// is rejected without force, a forced mutation transfers ownership by dropping the
// record, and a delete removes the row and record together.
func TestBlobStoreOwnershipOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			blobStore := domain.BlobStore{
				Name:             "archive",
				Driver:           "fs",
				ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_ARCHIVE"},
				PhysicalIdentity: strings.Repeat("a", 64),
			}

			// A declarative create adopts an ownership record.
			if err := metadata.SaveBlobStore(ctx, BlobStoreSave{
				BlobStore: blobStore,
				Create:    true,
				Ownership: Ownership{Declarative: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "blobStore", "archive"); err != nil {
				t.Fatalf("declarative create did not adopt record: %v", err)
			}

			// An imperative update of a managed store without force is rejected.
			if err := metadata.SaveBlobStore(ctx, BlobStoreSave{
				BlobStore: blobStore,
				Create:    false,
			}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative update of managed blob store = %v, want ErrManaged", err)
			}

			// An imperative update with force transfers ownership: the record is dropped.
			if err := metadata.SaveBlobStore(ctx, BlobStoreSave{
				BlobStore: blobStore,
				Create:    false,
				Ownership: Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "blobStore", "archive"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("force update did not release ownership: %v", err)
			}

			// An imperative delete of an unmanaged store succeeds and removes the row.
			if err := metadata.DeleteBlobStore(ctx, "archive", Ownership{}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.BlobStore(ctx, "archive"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("delete left the blob store: %v", err)
			}
		})
	}
}
