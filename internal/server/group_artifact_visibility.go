package server

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
)

// indexedGroupArtifactVisibility uses stored index state only. The repository
// route may refresh proxy metadata, but inventory reads must not make network
// requests or mutate the cache merely to decide which IDs are visible.
func (s *Server) indexedGroupArtifactVisibility(
	ctx context.Context, group domain.Repository, memberName string, asset domain.Asset,
) (visible, decided bool, err error) {
	if group.Type != "group" {
		return false, false, nil
	}
	owner, indexed, err := s.content.StoredGroupArtifactOwner(ctx, group, assetPublicPath(asset), asset.Path)
	if err != nil {
		return false, false, err
	}
	if !indexed {
		return false, false, nil
	}
	return owner == memberName, true, nil
}
