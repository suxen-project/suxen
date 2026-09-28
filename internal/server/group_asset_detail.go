package server

import (
	"context"
	"errors"

	"github.com/suxen-project/suxen/internal/domain"
)

// logicalAssetByID resolves an asset ID shown by group browse/components only
// while that asset is still visible through the group's ordered members. It
// never exposes a member repository name or a shadowed member asset.
func (s *Server) logicalAssetByID(ctx context.Context, repository domain.Repository, assetID int64) (domain.Asset, error) {
	asset, err := s.logicalStoredAssetByID(ctx, repository, assetID)
	if err == nil && repository.Type == "group" {
		asset.Repository = repository.Name
	}
	return asset, err
}

// logicalStoredAssetByID checks logical group visibility while preserving the
// member identity needed to apply its download policies to the stored bytes.
func (s *Server) logicalStoredAssetByID(ctx context.Context, repository domain.Repository, assetID int64) (domain.Asset, error) {
	if repository.Type != "group" {
		return s.metadata.ForRepository(repository).AssetByID(ctx, assetID)
	}
	sources, err := s.discoverySources(ctx, []domain.Repository{repository})
	if err != nil {
		return domain.Asset{}, err
	}
	for _, source := range sources {
		asset, err := s.metadata.ForRepository(source.leaf).AssetByID(ctx, assetID)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return domain.Asset{}, err
		}
		if visible, decided, err := s.indexedGroupArtifactVisibility(ctx, repository, source.leaf.Name, asset); err != nil {
			return domain.Asset{}, err
		} else if decided {
			if !visible {
				return domain.Asset{}, domain.ErrNotFound
			}
			return asset, nil
		}
		if repository.Format == "npm" {
			publicPath := assetPublicPath(asset)
			for _, priorID := range source.priorIDs {
				if _, err := s.priorMemberAssets().AssetByPublicPath(ctx, priorID, publicPath, 0); err == nil {
					return domain.Asset{}, domain.ErrNotFound
				} else if !errors.Is(err, domain.ErrNotFound) {
					return domain.Asset{}, err
				}
			}
			return asset, nil
		}
		for _, priorID := range source.priorIDs {
			if _, err := s.priorMemberAssets().AssetByRepositoryID(ctx, priorID, asset.Path); err == nil {
				return domain.Asset{}, domain.ErrNotFound
			} else if !errors.Is(err, domain.ErrNotFound) {
				return domain.Asset{}, err
			}
		}
		return asset, nil
	}
	return domain.Asset{}, domain.ErrNotFound
}
