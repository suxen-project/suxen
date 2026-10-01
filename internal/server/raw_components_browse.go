package server

import (
	"net/http"
	"sort"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/rawcomponent"
)

// writeRawComponents lists a Raw repository's component versions, one row per
// version with its member files. Rows are ordered by component, newest version
// first. Unmatched paths and versions without an anchor file are omitted, as
// they are not versions for retention either.
func (s *Server) writeRawComponents(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	rules rawcomponent.Rules,
) {
	assets, err := s.metadata.ForRepository(repository).Assets(r.Context(), "")
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	type versionKey struct {
		rule                     int
		name, version, directory string
	}
	rows := make(map[versionKey]*repositoryComponentVersion)
	anchored := make(map[versionKey]bool)
	for _, asset := range assets {
		match, ok := rules.Match(asset.Path)
		if !ok {
			continue
		}
		key := versionKey{rule: match.Rule, name: match.Name, version: match.Version, directory: match.Directory}
		row, found := rows[key]
		if !found {
			row = &repositoryComponentVersion{Component: match.Name, Format: repository.Format, Version: match.Version, Kind: asset.Kind}
			rows[key] = row
		}
		row.Assets = append(row.Assets, repositoryComponentAsset{
			AssetID: asset.ID, Path: asset.Path, Digest: asset.Digest, Size: asset.Size, UpdatedAt: asset.UpdatedAt,
		})
		row.Size += asset.Size
		if asset.UpdatedAt.After(row.UpdatedAt) {
			row.UpdatedAt = asset.UpdatedAt
		}
		if match.Anchor && (!anchored[key] || asset.Path < row.Reference) {
			row.AssetID, row.Digest, row.Reference = asset.ID, asset.Digest, asset.Path
			anchored[key] = true
		}
	}
	versions := make([]repositoryComponentVersion, 0, len(rows))
	for key, row := range rows {
		if !anchored[key] {
			continue
		}
		sort.Slice(row.Assets, func(i, j int) bool { return row.Assets[i].Path < row.Assets[j].Path })
		versions = append(versions, *row)
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Component != versions[j].Component {
			return versions[i].Component < versions[j].Component
		}
		if compared := compareVersions(versions[i].Version, versions[j].Version); compared != 0 {
			return compared > 0
		}
		return versions[i].Reference < versions[j].Reference
	})
	httpx.WriteCollection(w, r, "repository-components:"+repository.ID, versions,
		func(row repositoryComponentVersion) string {
			return row.Component + "\x00" + row.Version + "\x00" + row.Reference
		})
}
