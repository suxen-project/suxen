package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestDeleteAssetWithCompanionsByRepositoryID verifies the store primitive both
// interactive-delete handlers share: the artifact is deleted by path (a missing
// path is ErrNotFound), its declared companion metadata records are removed in the
// same transaction, and a non-metadata asset that happens to sit at a declared
// companion path is left untouched.
func TestDeleteAssetWithCompanionsByRepositoryID(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "r", Format: "raw", Type: "hosted", Writable: true,
			}); err != nil {
				t.Fatal(err)
			}
			repository, err := metadata.Repository(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			put := func(path, kind, digest string) {
				if _, err := metadata.PutAsset(ctx, domain.Asset{
					Repository: "r", Path: path, Kind: kind, Digest: digest, Size: 1,
				}); err != nil {
					t.Fatal(err)
				}
			}
			digestA := "sha256:" + strings.Repeat("a", 64)
			digestB := "sha256:" + strings.Repeat("b", 64)
			digestC := "sha256:" + strings.Repeat("c", 64)

			put("a/artifact", "raw", digestA)
			put("a/meta.json", "metadata", digestB)
			// A non-metadata asset sharing a declared companion path must survive.
			put("a/keep", "raw", digestC)

			deleted, err := metadata.DeleteAssetWithCompanionsByRepositoryID(
				ctx, repository.ID, "r", "a/artifact", []string{"a/meta.json", "a/keep"})
			if err != nil {
				t.Fatal(err)
			}
			if deleted.Path != "a/artifact" {
				t.Fatalf("returned asset path = %q, want a/artifact", deleted.Path)
			}
			if _, err := metadata.Asset(ctx, "r", "a/artifact"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("artifact survived: %v", err)
			}
			if _, err := metadata.Asset(ctx, "r", "a/meta.json"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("metadata companion survived: %v", err)
			}
			if _, err := metadata.Asset(ctx, "r", "a/keep"); err != nil {
				t.Fatalf("non-metadata asset at a declared companion path was deleted: %v", err)
			}

			// A missing artifact path is ErrNotFound and removes nothing.
			if _, err := metadata.DeleteAssetWithCompanionsByRepositoryID(
				ctx, repository.ID, "r", "a/missing", []string{"a/keep"}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("missing path error = %v, want ErrNotFound", err)
			}
			if _, err := metadata.Asset(ctx, "r", "a/keep"); err != nil {
				t.Fatalf("companion removed for a missing artifact: %v", err)
			}
		})
	}
}
