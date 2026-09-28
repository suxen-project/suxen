package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestDeleteAssetByIDWithCompanionsByRepositoryID(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: "r", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: "other", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			repository, err := metadata.Repository(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			other, err := metadata.Repository(ctx, "other")
			if err != nil {
				t.Fatal(err)
			}
			put := func(repository, path, kind, digit string) domain.Asset {
				t.Helper()
				asset, err := metadata.PutAsset(ctx, domain.Asset{
					Repository: repository, Path: path, Kind: kind, Digest: "sha256:" + strings.Repeat(digit, 64),
				})
				if err != nil {
					t.Fatal(err)
				}
				return asset
			}
			old := put("r", "artifact", "raw", "a")
			put("r", "meta.json", "metadata", "b")
			replacement := put("r", "artifact", "raw", "c")
			if replacement.ID == old.ID {
				t.Fatal("asset replacement reused ID")
			}
			if _, err := metadata.DeleteAssetByIDWithCompanionsByRepositoryID(ctx, repository.ID, "r", old.ID, []string{"meta.json"}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("stale generation delete = %v, want ErrNotFound", err)
			}
			if asset, err := metadata.Asset(ctx, "r", "artifact"); err != nil || asset.ID != replacement.ID {
				t.Fatalf("replacement lost: %+v, %v", asset, err)
			}
			if _, err := metadata.Asset(ctx, "r", "meta.json"); err != nil {
				t.Fatalf("companion lost on stale delete: %v", err)
			}
			if _, err := metadata.DeleteAssetByIDWithCompanionsByRepositoryID(ctx, other.ID, "other", replacement.ID, []string{"meta.json"}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("wrong-repository delete = %v, want ErrNotFound", err)
			}
			deleted, err := metadata.DeleteAssetByIDWithCompanionsByRepositoryID(ctx, repository.ID, "r", replacement.ID, []string{"meta.json"})
			if err != nil || deleted.ID != replacement.ID || deleted.RepositoryID != repository.ID {
				t.Fatalf("successful delete = %+v, %v", deleted, err)
			}
			for _, path := range []string{"artifact", "meta.json"} {
				if _, err := metadata.Asset(ctx, "r", path); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("%s survived: %v", path, err)
				}
			}
		})
	}
}
