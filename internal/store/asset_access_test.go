package store

import (
	"context"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestSQLiteUploadCountsAsAccess verifies that creating an asset (an upload) sets
// last_accessed, so a never-downloaded asset still has a non-nil access time for
// "not accessed since" predicates to compare against.
func TestSQLiteUploadCountsAsAccess(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "repo", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "repo",
		Path:       "artifact",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.LastAccessed == nil {
		t.Fatal("upload did not set LastAccessed")
	}
	if !asset.LastAccessed.Equal(asset.CreatedAt) {
		t.Fatalf("LastAccessed = %v, want createdAt %v", asset.LastAccessed, asset.CreatedAt)
	}

	// A later download advances it.
	accessed := asset.CreatedAt.Add(time.Hour)
	if err := metadata.TouchAsset(ctx, asset.ID, accessed); err != nil {
		t.Fatal(err)
	}
	touched, err := metadata.Asset(ctx, "repo", "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if touched.LastAccessed == nil || !touched.LastAccessed.Equal(accessed) {
		t.Fatalf("after download LastAccessed = %v, want %v", touched.LastAccessed, accessed)
	}
}

func TestAssetAccessTimeNeverMovesBackward(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: "access-order", Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatal(err)
		}
		asset, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: "access-order", Path: "artifact",
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
		if err != nil {
			t.Fatal(err)
		}
		repository, err := metadata.Repository(ctx, "access-order")
		if err != nil {
			t.Fatal(err)
		}
		newer := asset.CreatedAt.Add(2 * time.Hour)
		older := asset.CreatedAt.Add(time.Hour)
		for _, touches := range []struct {
			name  string
			first func(time.Time) error
			last  func(time.Time) error
		}{
			{"global then scoped", func(at time.Time) error { return metadata.TouchAsset(ctx, asset.ID, at) },
				func(at time.Time) error { return metadata.TouchAssetByRepositoryID(ctx, repository.ID, asset.ID, at) }},
			{"scoped then global", func(at time.Time) error { return metadata.TouchAssetByRepositoryID(ctx, repository.ID, asset.ID, at) },
				func(at time.Time) error { return metadata.TouchAsset(ctx, asset.ID, at) }},
		} {
			t.Run(touches.name, func(t *testing.T) {
				if err := touches.first(newer); err != nil {
					t.Fatal(err)
				}
				if err := touches.last(older); err != nil {
					t.Fatal(err)
				}
				stored, err := metadata.Asset(ctx, "access-order", asset.Path)
				if err != nil {
					t.Fatal(err)
				}
				if stored.LastAccessed == nil || !stored.LastAccessed.Equal(newer) {
					t.Fatalf("last access = %v, want latest successful access %v", stored.LastAccessed, newer)
				}
			})
		}
		// RFC3339Nano uses variable fractional width, so a TEXT comparison
		// incorrectly orders a whole second after a later fractional instant.
		fractional := asset.CreatedAt.Truncate(time.Second).Add(3*time.Hour + 200*time.Millisecond)
		if err := metadata.TouchAsset(ctx, asset.ID, fractional); err != nil {
			t.Fatal(err)
		}
		if err := metadata.TouchAssetByRepositoryID(ctx, repository.ID, asset.ID, fractional.Truncate(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := metadata.TouchAssetByRepositoryID(ctx, "", asset.ID, fractional.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := metadata.TouchAssetByRepositoryID(ctx, "missing-repository-id", asset.ID, fractional.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		stored, err := metadata.Asset(ctx, "access-order", asset.Path)
		if err != nil {
			t.Fatal(err)
		}
		if stored.LastAccessed == nil || !stored.LastAccessed.Equal(fractional) {
			t.Fatalf("access after fractional and empty-scope touches = %v, want %v", stored.LastAccessed, fractional)
		}
		// A same-digest publication keeps the asset generation and must not
		// replace a later recorded download with its earlier publication time.
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: "access-order", Path: asset.Path, Digest: asset.Digest,
		}); err != nil {
			t.Fatal(err)
		}
		stored, err = metadata.Asset(ctx, "access-order", asset.Path)
		if err != nil {
			t.Fatal(err)
		}
		if stored.LastAccessed == nil || !stored.LastAccessed.Equal(fractional) {
			t.Fatalf("access after same-digest publication = %v, want %v", stored.LastAccessed, fractional)
		}
	})
}
