package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/rawcomponent"
)

// rawComponentCursor resumes after the component of asset AfterID. Resource
// binds it to the repository and its patterns, so a pattern change makes
// outstanding cursors stale.
type rawComponentCursor struct {
	Version  int    `json:"v"`
	Resource string `json:"r"`
	AfterID  int64  `json:"a"`
}

// writeRawComponents lists a Raw repository's component versions, one row per
// version with its member files. Pages hold limit components with all their
// versions, ordered by component and then newest version first. Unmatched
// paths and versions without an anchor file are omitted, as they are not
// versions for retention either.
func (s *Server) writeRawComponents(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	rules rawcomponent.Rules,
) {
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	resource := rawComponentResource(repository)
	cursor := rawComponentCursor{Version: 1, Resource: resource}
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		var supplied rawComponentCursor
		if err := httpx.DecodeCursor(encoded, &supplied); err != nil || supplied.Version != 1 || supplied.AfterID < 1 {
			httpx.WriteCursorError(w, errors.New("cursor payload is invalid"))
			return
		}
		if supplied.Resource != resource {
			httpx.WriteCursorError(w, &httpx.StaleCursorError{Reason: "cursor belongs to a different collection"})
			return
		}
		cursor = supplied
	}
	page, err := s.metadata.ForRepository(repository).ComponentPage(r.Context(), cursor.AfterID, limit)
	if errors.Is(err, domain.ErrNotFound) {
		httpx.WriteCursorError(w, &httpx.StaleCursorError{Reason: "collection changed after the cursor was issued"})
		return
	}
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	versions, lastID := rawComponentVersions(repository, rules, page.Assets)
	result := httpx.CollectionPage[repositoryComponentVersion]{Items: versions}
	if page.HasMore && lastID > 0 {
		result.NextCursor = httpx.EncodeCursor(rawComponentCursor{Version: 1, Resource: resource, AfterID: lastID})
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func rawComponentResource(repository domain.Repository) string {
	encoded, _ := json.Marshal(repository.FormatConfig)
	digest := sha256.Sum256(encoded)
	return "repository-components:" + repository.ID + ":" + base64.RawURLEncoding.EncodeToString(digest[:12])
}

// rawComponentVersions groups one page of component rows into version rows and
// returns the ID of an asset in the page's last component for the next cursor.
func rawComponentVersions(
	repository domain.Repository,
	rules rawcomponent.Rules,
	assets []domain.Asset,
) ([]repositoryComponentVersion, int64) {
	type versionKey struct {
		rule                     int
		name, version, directory string
	}
	rows := make(map[versionKey]*repositoryComponentVersion)
	anchored := make(map[versionKey]bool)
	var lastComponent string
	var lastID int64
	for _, asset := range assets {
		if asset.Component >= lastComponent {
			lastComponent, lastID = asset.Component, asset.ID
		}
		match, ok := rules.Match(assetPublicPath(asset))
		if !ok || match.Name != asset.Component {
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
	return versions, lastID
}
