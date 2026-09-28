package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/gomod"
)

func putGoUnitTestAsset(t *testing.T, metadata *store.SQLStore, version, extension string) domain.Asset {
	t.Helper()
	asset, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "go-hosted", Path: "example.com/widget/@v/" + version + "." + extension,
		Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestGoCleanupKeepLastCountsCompleteVersions(t *testing.T) {
	for _, repositoryType := range []string{"hosted", "proxy"} {
		t.Run(repositoryType, func(t *testing.T) {
			f := newServerFixture(t)
			ctx := context.Background()
			if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "go-hosted", Format: "go", Type: repositoryType, Upstream: "https://proxy.example"}); err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"v1.0.0", "v1.1.0"} {
				for _, extension := range []string{"info", "mod", "zip"} {
					putGoUnitTestAsset(t, f.Metadata, version, extension)
				}
			}
			policy := domain.CleanupPolicy{Name: "keep-one", KeepLast: 1,
				Criteria: domain.CleanupCriteria{{Path: "go.module", Op: "=", Value: "example.com/widget"}}}
			preview, err := f.Handler.cleanupRepository(ctx, policy, "go-hosted", true, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if preview.Matched != 3 || !containsAll(preview.WouldDelete,
				"example.com/widget/@v/v1.0.0.info", "example.com/widget/@v/v1.0.0.mod", "example.com/widget/@v/v1.0.0.zip") {
				t.Fatalf("preview = %+v", preview)
			}
			result, err := f.Handler.cleanupRepository(ctx, policy, "go-hosted", false, time.Now())
			if err != nil || result.Deleted != 3 {
				t.Fatalf("cleanup = %+v, %v", result, err)
			}
			for _, extension := range []string{"info", "mod", "zip"} {
				if _, err := f.Metadata.Asset(ctx, "go-hosted", "example.com/widget/@v/v1.0.0."+extension); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("old %s remained: %v", extension, err)
				}
				if _, err := f.Metadata.Asset(ctx, "go-hosted", "example.com/widget/@v/v1.1.0."+extension); err != nil {
					t.Fatalf("new %s lost: %v", extension, err)
				}
			}
		})
	}
}

func TestGoCleanupRequiresCompleteAndFullyMatchingUnit(t *testing.T) {
	f := newServerFixture(t)
	ctx := context.Background()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "go-hosted", Format: "go", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	for _, extension := range []string{"info", "mod", "zip"} {
		putGoUnitTestAsset(t, f.Metadata, "v1.0.0", extension)
	}
	for _, extension := range []string{"info", "mod"} {
		putGoUnitTestAsset(t, f.Metadata, "v1.1.0", extension)
	}
	policy := domain.CleanupPolicy{Name: "sweep", Criteria: domain.CleanupCriteria{{Path: "go.module", Op: "=", Value: "example.com/widget"}}}
	for _, version := range []string{"v1.0.0", "v1.1.0"} {
		for _, extension := range []string{"info", "mod", "zip"} {
			assetPath := "example.com/widget/@v/" + version + "." + extension
			asset, err := f.Metadata.Asset(ctx, "go-hosted", assetPath)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Metadata.SetAttributes(ctx, "go-hosted", asset.ID, "retention", map[string]any{"keep": false}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.Metadata.SetAttributes(ctx, "go-hosted", assetID(t, f.Metadata, "example.com/widget/@v/v1.0.0.zip"), "retention", map[string]any{"keep": true}); err != nil {
		t.Fatal(err)
	}
	policy.Criteria = append(policy.Criteria, domain.Predicate{Path: "retention.keep", Op: "=", Value: false})
	result, err := f.Handler.cleanupRepository(ctx, policy, "go-hosted", false, time.Now())
	if err != nil || result.Deleted != 0 {
		t.Fatalf("partial match cleanup = %+v, %v", result, err)
	}
	policy.Criteria = policy.Criteria[:1]
	result, err = f.Handler.cleanupRepository(ctx, policy, "go-hosted", false, time.Now())
	if err != nil || result.Deleted != 3 {
		t.Fatalf("complete unit cleanup = %+v, %v", result, err)
	}
	for _, extension := range []string{"info", "mod"} {
		if _, err := f.Metadata.Asset(ctx, "go-hosted", "example.com/widget/@v/v1.1.0."+extension); err != nil {
			t.Fatalf("incomplete unit %s lost: %v", extension, err)
		}
	}
}

func assetID(t *testing.T, metadata *store.SQLStore, assetPath string) int64 {
	t.Helper()
	asset, err := metadata.Asset(context.Background(), "go-hosted", assetPath)
	if err != nil {
		t.Fatal(err)
	}
	return asset.ID
}

type auditGoUnitStore struct {
	store.Store
	beforeDelete func(context.Context, []domain.Asset) error
}

func (s *auditGoUnitStore) DeleteAssetsIfUnchanged(ctx context.Context, assets []domain.Asset) (bool, error) {
	if err := s.beforeDelete(ctx, assets); err != nil {
		return false, err
	}
	return s.Store.DeleteAssetsIfUnchanged(ctx, assets)
}

func TestGoCleanupConcurrentPeerChangePreservesWholeUnit(t *testing.T) {
	for _, change := range []string{"attribute", "access"} {
		t.Run(change, func(t *testing.T) {
			f := newServerFixture(t)
			ctx := context.Background()
			if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "go-hosted", Format: "go", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			for _, extension := range []string{"info", "mod", "zip"} {
				putGoUnitTestAsset(t, f.Metadata, "v1.0.0", extension)
			}
			changed := false
			f.Handler.metadata = &auditGoUnitStore{Store: f.Metadata, beforeDelete: func(ctx context.Context, _ []domain.Asset) error {
				if changed {
					return nil
				}
				changed = true
				id := assetID(t, f.Metadata, "example.com/widget/@v/v1.0.0.zip")
				if change == "access" {
					return f.Metadata.TouchAsset(ctx, id, time.Now().UTC().Add(time.Minute))
				}
				return f.Metadata.SetAttributes(ctx, "go-hosted", id, "retention", map[string]any{"keep": true})
			}}
			policy := domain.CleanupPolicy{Name: "sweep", Criteria: domain.CleanupCriteria{{Path: "go.module", Op: "=", Value: "example.com/widget"}}}
			result, err := f.Handler.cleanupRepository(ctx, policy, "go-hosted", false, time.Now())
			if err != nil || result.Deleted != 0 || result.SkippedChanged != 3 || !changed {
				t.Fatalf("concurrent cleanup = %+v, %v; changed=%v", result, err, changed)
			}
			for _, extension := range []string{"info", "mod", "zip"} {
				if _, err := f.Metadata.Asset(ctx, "go-hosted", "example.com/widget/@v/v1.0.0."+extension); err != nil {
					t.Fatalf("peer %s lost: %v", extension, err)
				}
			}
		})
	}
}
