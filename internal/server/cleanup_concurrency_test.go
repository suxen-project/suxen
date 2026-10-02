package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type auditCleanupMutationStore struct {
	store.Store
	beforeDelete func(context.Context, domain.Asset) error
}

func (s *auditCleanupMutationStore) DeleteAssetWithCompanions(ctx context.Context, artifact domain.Asset, companionPaths []string) (bool, error) {
	if err := s.beforeDelete(ctx, artifact); err != nil {
		return false, err
	}
	return s.Store.DeleteAssetWithCompanions(ctx, artifact, companionPaths)
}

func (s *auditCleanupMutationStore) DeleteAssetsIfUnchanged(ctx context.Context, assets []domain.Asset) (bool, error) {
	for _, asset := range assets {
		if err := s.beforeDelete(ctx, asset); err != nil {
			return false, err
		}
	}
	return s.Store.DeleteAssetsIfUnchanged(ctx, assets)
}

func (s *auditCleanupMutationStore) DeleteAssetSetsIfUnchanged(ctx context.Context, formatConfig map[string]any, sets []store.AssetSet, assets []domain.Asset) (bool, error) {
	for _, asset := range assets {
		if err := s.beforeDelete(ctx, asset); err != nil {
			return false, err
		}
	}
	return s.Store.DeleteAssetSetsIfUnchanged(ctx, formatConfig, sets, assets)
}

func TestAuditCleanupRechecksPolicyAfterConcurrentMetadataChange(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"download", "retention-attribute"} {
		t.Run(change, func(t *testing.T) {
			f := newServerFixture(t)
			ctx := context.Background()
			asset, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "raw", Path: "archive.bin", Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1, Attributes: map[string]any{"retention": map[string]any{"keep": false}}})
			if err != nil {
				t.Fatal(err)
			}
			// Evaluate cleanup later, rather than moving access time backwards:
			// download bookkeeping deliberately preserves the latest access.
			now := asset.CreatedAt.Add(60 * 24 * time.Hour)
			criteria := domain.CleanupCriteria{{Path: "sys.lastAccessed", Op: "before", Value: "30d"}}
			if change == "retention-attribute" {
				criteria = domain.CleanupCriteria{{Path: "retention.keep", Op: "=", Value: false}}
			}
			policy := domain.CleanupPolicy{Name: "sweep", Repositories: []string{"raw"}, Criteria: criteria, Action: "delete"}
			repository, err := f.Metadata.Repository(ctx, "raw")
			if err != nil {
				t.Fatal(err)
			}
			f.Handler.metadata = &auditCleanupMutationStore{Store: f.Metadata, beforeDelete: func(ctx context.Context, artifact domain.Asset) error {
				var err error
				if change == "download" {
					err = f.Metadata.TouchAsset(ctx, asset.ID, now)
				} else {
					err = f.Metadata.SetAttributes(ctx, "raw", asset.ID, "retention", map[string]any{"keep": true})
				}
				if err != nil {
					return err
				}
				current, err := f.Metadata.Asset(ctx, "raw", asset.Path)
				if err != nil {
					return err
				}
				if assetMatchesCleanupCriteria(current, repository, criteria, now) {
					t.Fatal("probe failed: changed asset still matches")
				}
				if current.Digest != artifact.Digest || !current.UpdatedAt.Equal(artifact.UpdatedAt) {
					t.Fatal("test did not isolate access or attribute mutation")
				}
				return nil
			}}
			result, err := f.Handler.cleanupRepository(ctx, policy, "raw", false, now)
			if err != nil {
				t.Fatal(err)
			}
			if result.Deleted != 0 || result.SkippedChanged != 1 {
				t.Fatalf("deleted newly protected/accessed asset: result=%+v", result)
			}
		})
	}
}
