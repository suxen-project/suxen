package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// maxManifestBytes caps how much of a manifest body the drill-in endpoint reads.
// OCI manifests are small JSON documents; the cap guards against a pathological
// or corrupt blob being streamed whole into memory.
const maxManifestBytes = 8 << 20

// repositoryComponentVersion is one bounded browse row. Components may span
// pages when an image or package has more versions than the requested limit.
type repositoryComponentVersion struct {
	Component string    `json:"component"`
	Format    string    `json:"format"`
	Version   string    `json:"version"`
	Reference string    `json:"reference"`
	AssetID   int64     `json:"assetId"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Kind      string    `json:"kind"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Server) handleRepositoryComponents(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	sources, err := s.discoverySources(r.Context(), []domain.Repository{repository})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	resource := discoveryResource("repository-components:"+repository.ID, sources)
	cursor, err := parseDiscoveryCursor(r, resource, len(sources))
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	items, next, err := s.scanDiscovery(r.Context(), sources, cursor, "", limit,
		func(root domain.Repository, asset domain.Asset) (repositoryBrowseItem, bool) {
			// OCI layer blobs are reached through manifests, never shown as
			// versions in the tags-first browse view.
			if root.Format == "oci" && asset.Kind != "oci-manifest" {
				return repositoryBrowseItem{}, false
			}
			asset.Repository = root.Name
			return browseItems(root, []domain.Asset{asset})[0], true
		})
	if err != nil {
		var stale *httpx.StaleCursorError
		if errors.As(err, &stale) {
			httpx.WriteCursorError(w, stale)
		} else {
			httpx.WriteResult(w, nil, err)
		}
		return
	}
	versions := make([]repositoryComponentVersion, 0, len(items))
	for _, item := range items {
		versions = append(versions, repositoryComponentVersion{
			Component: item.Component,
			Format:    item.Format,
			Version:   item.Version,
			Reference: item.Asset.Reference,
			AssetID:   item.Asset.ID,
			Digest:    item.Asset.Digest,
			Size:      item.Asset.Size,
			Kind:      item.Asset.Kind,
			UpdatedAt: item.Asset.UpdatedAt,
		})
	}
	page := httpx.CollectionPage[repositoryComponentVersion]{Items: versions}
	if next != nil {
		page.NextCursor = httpx.EncodeCursor(*next)
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

// manifestDescriptor is one referenced object (config, layer, sub-manifest, or
// subject) of an OCI manifest.
type manifestDescriptor struct {
	MediaType    string `json:"mediaType,omitempty"`
	ArtifactType string `json:"artifactType,omitempty"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
}

// manifestContents is the parsed config/layers view of one manifest, read from
// the stored blob body since suxen persists only the dependency digests, not the
// per-descriptor sizes and media types the UI shows.
type manifestContents struct {
	MediaType    string               `json:"mediaType,omitempty"`
	ArtifactType string               `json:"artifactType,omitempty"`
	Digest       string               `json:"digest"`
	Size         int64                `json:"size"`
	Config       *manifestDescriptor  `json:"config,omitempty"`
	Layers       []manifestDescriptor `json:"layers,omitempty"`
	Manifests    []manifestDescriptor `json:"manifests,omitempty"`
	Subject      *manifestDescriptor  `json:"subject,omitempty"`
}

func (s *Server) handleAssetManifest(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	assetIDValue string,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	assetID, err := strconv.ParseInt(assetIDValue, 10, 64)
	if err != nil || assetID <= 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_id", "asset ID must be positive")
		return
	}
	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	asset, err := s.logicalAssetByID(r.Context(), repository, assetID)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if asset.Kind != "oci-manifest" {
		httpx.WriteProblem(
			w, http.StatusBadRequest, "not_a_manifest",
			"asset is not an OCI manifest",
		)
		return
	}
	reader, _, err := s.content.OpenStoredAsset(r.Context(), asset)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	defer reader.Close()

	body, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes))
	if err != nil {
		httpx.WriteServerProblem(
			w, http.StatusInternalServerError, "manifest_read_error",
			"read manifest body failed", err,
		)
		return
	}
	var envelope struct {
		MediaType    string               `json:"mediaType"`
		ArtifactType string               `json:"artifactType"`
		Config       *manifestDescriptor  `json:"config"`
		Layers       []manifestDescriptor `json:"layers"`
		Manifests    []manifestDescriptor `json:"manifests"`
		Subject      *manifestDescriptor  `json:"subject"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		httpx.WriteProblem(
			w, http.StatusUnprocessableEntity, "manifest_parse_error",
			"stored manifest body is not valid JSON",
		)
		return
	}
	mediaType := envelope.MediaType
	if mediaType == "" {
		mediaType = asset.ContentType
	}
	httpx.WriteJSON(w, http.StatusOK, manifestContents{
		MediaType:    mediaType,
		ArtifactType: envelope.ArtifactType,
		Digest:       asset.Digest,
		Size:         asset.Size,
		Config:       envelope.Config,
		Layers:       envelope.Layers,
		Manifests:    envelope.Manifests,
		Subject:      envelope.Subject,
	})
}
