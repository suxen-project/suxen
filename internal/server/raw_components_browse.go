package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	"github.com/suxen-project/suxen/internal/store"
)

// rawComponentCursor resumes after the last component version of a page.
// Resource binds it to the repository and its patterns, so a pattern change
// makes outstanding cursors stale.
type rawComponentCursor struct {
	Version    int    `json:"v"`
	Resource   string `json:"r"`
	Component  string `json:"c"`
	VersionKey string `json:"k"`
	Name       string `json:"n"`
}

// writeRawComponents lists a Raw repository's component versions, one row per
// version with its member files, ordered by component and then highest
// version first. limit counts stored component versions. A version without an
// anchor file is omitted, as it is not a version for retention either, so a
// page can hold fewer rows than limit.
func (s *Server) writeRawComponents(w http.ResponseWriter, r *http.Request, repository domain.Repository) {
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	resource := rawComponentResource(repository)
	after, err := parseRawComponentCursor(r, resource)
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	page, err := s.metadata.ForRepository(repository).ComponentVersionPage(r.Context(), after, limit)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	result := httpx.CollectionPage[repositoryComponentVersion]{
		Items: rawComponentVersions(repository, page.Assets),
	}
	if page.HasMore {
		last := page.Versions[len(page.Versions)-1]
		result.NextCursor = httpx.EncodeCursor(rawComponentCursor{
			Version: 1, Resource: resource, Component: last.Component, VersionKey: last.VersionKey, Name: last.Version,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

// parseRawComponentCursor accepts the same parameters as the discovery
// listings: no page, and at most one nonempty cursor.
func parseRawComponentCursor(r *http.Request, resource string) (*store.ComponentVersion, error) {
	query := r.URL.Query()
	if query.Has("page") {
		return nil, errors.New("page is not supported for component listings; use cursor")
	}
	values, found := query["cursor"]
	if !found {
		return nil, nil
	}
	if len(values) != 1 || values[0] == "" {
		return nil, errors.New("cursor length is invalid")
	}
	var supplied rawComponentCursor
	if err := httpx.DecodeCursor(values[0], &supplied); err != nil || supplied.Version != 1 || supplied.Component == "" {
		return nil, errors.New("cursor payload is invalid")
	}
	if supplied.Resource != resource {
		return nil, &httpx.StaleCursorError{Reason: "cursor belongs to a different collection"}
	}
	return &store.ComponentVersion{Component: supplied.Component, VersionKey: supplied.VersionKey, Version: supplied.Name}, nil
}

func rawComponentResource(repository domain.Repository) string {
	encoded, _ := json.Marshal(repository.FormatConfig)
	digest := sha256.Sum256(encoded)
	return "repository-components:" + repository.ID + ":" + base64.RawURLEncoding.EncodeToString(digest[:12])
}

// rawComponentVersions groups one page's rows, already in listing order, into
// version rows: one per component version, whichever rules or version
// directories hold its files, as retention counts it. Unmatched files form
// their own rows even when a pattern component has the same name.
func rawComponentVersions(repository domain.Repository, assets []domain.Asset) []repositoryComponentVersion {
	type versionKey struct {
		implicit      bool
		name, version string
	}
	rules := rawcomponent.ForConfig(repository.FormatConfig)
	rows := make(map[versionKey]*repositoryComponentVersion)
	anchored := make(map[versionKey]bool)
	var order []versionKey
	for _, asset := range assets {
		match := rules.Match(asset.Path)
		key := versionKey{match.Implicit, match.Name, match.Version}
		row, found := rows[key]
		if !found {
			row = &repositoryComponentVersion{Component: match.Name, Format: repository.Format, Version: match.Version, Kind: asset.Kind}
			rows[key] = row
			order = append(order, key)
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
	versions := make([]repositoryComponentVersion, 0, len(order))
	for _, key := range order {
		if anchored[key] {
			versions = append(versions, *rows[key])
		}
	}
	return versions
}
