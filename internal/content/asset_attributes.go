package content

import (
	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
)

func ProjectAsset(asset domain.Asset, repository domain.Repository) domain.Asset {
	return projectAsset(asset, repository)
}

func ProjectAssets(assets []domain.Asset, repository domain.Repository) []domain.Asset {
	return projectAssets(assets, repository)
}

func projectAsset(asset domain.Asset, repository domain.Repository) domain.Asset {
	asset.Attributes = assetattrs.Project(asset, repository)
	return asset
}

func projectAssets(assets []domain.Asset, repository domain.Repository) []domain.Asset {
	projected := make([]domain.Asset, len(assets))
	for index, asset := range assets {
		projected[index] = projectAsset(asset, repository)
	}
	return projected
}
