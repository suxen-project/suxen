package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/retention"
)

func (s *Server) handleAssets(
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
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if repository.Type == "group" {
		s.handleGroupAssets(w, r, repository, prefix, limit)
		return
	}
	request, err := parseAssetPageRequest(r, repository.ID, prefix, limit)
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	result, err := s.metadata.ForRepository(repository).AssetPage(r.Context(), request)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) && request.AfterID > 0 {
			httpx.WriteProblem(w, http.StatusConflict, "stale_cursor", "cursor anchor no longer exists")
			return
		}
		httpx.WriteResult(w, nil, err)
		return
	}
	projected := content.ProjectAssets(result.Items, repository)
	page := httpx.CollectionPage[domain.Asset]{Items: projected}
	if result.HasMore && len(result.Items) > 0 {
		page.NextCursor = httpx.EncodeCursor(assetPageCursor{
			Version:  1,
			Resource: assetPageResource(repository.ID, prefix),
			AfterID:  result.Items[len(result.Items)-1].ID,
			MaxID:    result.MaxID,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) handleGroupAssets(
	w http.ResponseWriter, r *http.Request, repository domain.Repository, prefix string, limit int,
) {
	sources, err := s.discoverySources(r.Context(), []domain.Repository{repository})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	resource := discoveryResource("repository-assets:"+repository.ID+":"+prefix, sources)
	cursor, err := parseDiscoveryCursor(r, resource, len(sources))
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	items, next, err := s.scanDiscovery(r.Context(), sources, cursor, prefix, limit,
		func(root domain.Repository, asset domain.Asset) (repositoryBrowseItem, bool) {
			asset.Repository = root.Name
			asset = content.ProjectAsset(asset, root)
			return repositoryBrowseItem{Asset: asset}, true
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
	assets := make([]domain.Asset, 0, len(items))
	for _, item := range items {
		assets = append(assets, item.Asset)
	}
	page := httpx.CollectionPage[domain.Asset]{Items: assets}
	if next != nil {
		page.NextCursor = httpx.EncodeCursor(*next)
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) handleAssetAttributes(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	assetIDValue string,
	namespace string,
) {
	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	metadata := s.metadata.ForRepository(repository)

	assetID, err := strconv.ParseInt(assetIDValue, 10, 64)
	if err != nil || assetID <= 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_id", "asset ID must be positive")
		return
	}

	if namespace == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_namespace", "namespace is required")
		return
	}
	if assetattrs.IsReservedNamespace(namespace) && r.Method != http.MethodGet {
		httpx.WriteProblem(
			w,
			http.StatusForbidden,
			"reserved_namespace",
			"the namespace is reserved for system-managed attributes",
		)
		return
	}
	if repository.Type == "group" && r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	switch r.Method {
	case http.MethodGet:
		asset, err := s.logicalAssetByID(r.Context(), repository, assetID)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		asset = content.ProjectAsset(asset, repository)
		value, found := asset.Attributes[namespace]
		if !found {
			httpx.WriteResult(w, nil, domain.ErrNotFound)
			return
		}
		w.Header().Set("ETag", content.QuoteETag(asset.Digest))
		httpx.WriteJSON(w, http.StatusOK, value)
	case http.MethodPut:
		var value map[string]any
		if !httpx.DecodeJSON(w, r, &value) {
			return
		}
		asset, err := metadata.AssetByID(r.Context(), assetID)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if !requireAssetGeneration(w, r, asset) {
			return
		}
		_, replacing := asset.Attributes[namespace]
		err = metadata.SetAttributes(
			r.Context(),
			assetID,
			namespace,
			value,
		)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if !replacing {
			httpx.WriteCreated(w, r.URL.Path, value)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, value)
	case http.MethodDelete:
		asset, err := metadata.AssetByID(r.Context(), assetID)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if !requireAssetGeneration(w, r, asset) {
			return
		}
		err = metadata.DeleteAttributes(
			r.Context(),
			assetID,
			namespace,
		)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func requireAssetGeneration(w http.ResponseWriter, r *http.Request, asset domain.Asset) bool {
	values := r.Header.Values("If-Match")
	precondition := strings.Join(values, ", ")
	if precondition == "" {
		httpx.WriteProblem(
			w, http.StatusPreconditionRequired, "asset_precondition_required",
			"If-Match with the current asset digest is required",
		)
		return false
	}
	// Preserve the legacy bare digest only as a single complete field value.
	// A tag list must contain the current strong ETag; * and weak tags cannot
	// prove that the scanner inspected this generation's bytes.
	bareDigest := len(values) == 1 && strings.TrimSpace(values[0]) == asset.Digest
	if !bareDigest && !content.StrongETagListContains(precondition, content.QuoteETag(asset.Digest)) {
		httpx.WriteProblem(
			w, http.StatusPreconditionFailed, "asset_generation_changed",
			"the asset content changed before the annotation was applied",
		)
		return false
	}
	return true
}

func (s *Server) handleAssetItem(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	assetIDValue string,
) {
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
	if repository.Type == "group" && r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	metadata := s.metadata.ForRepository(repository)
	asset, err := s.logicalAssetByID(r.Context(), repository, assetID)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}

	switch r.Method {
	case http.MethodGet:
		httpx.WriteJSON(w, http.StatusOK, content.ProjectAsset(asset, repository))
	case http.MethodDelete:
		// Remove the artifact's declared companion metadata in the same
		// transaction so an interactive delete cannot orphan a companion record
		// and leak its blob the way policy cleanup already avoids. Fail closed on
		// an invalid declaration.
		companionPaths, ok := s.declaredCompanionPaths(repository, retention.FormatGrouping(repository.Format), asset.Path)
		if !ok {
			httpx.WriteProblem(w, http.StatusInternalServerError, "invalid_companion_paths",
				"the repository format declared invalid companion paths for this asset")
			return
		}
		deleted, err := metadata.DeleteAssetByIDWithCompanions(r.Context(), assetID, companionPaths)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if deleted.Kind == "oci-manifest" {
			if _, err := s.manifestAliasCleanup().DeleteDanglingManifestAliases(
				r.Context(),
				repositoryName,
				[]domain.Asset{deleted},
			); err != nil {
				httpx.WriteResult(w, nil, err)
				return
			}
		}
		s.content.EnqueueAssetEvent(r.Context(), domain.WebhookAssetDeleted, deleted)
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

// handleAssetDownload serves the stored generation selected by its visible
// asset ID. It does not reconstruct a proxy URL from a cache key: some formats
// intentionally hash secret URL query values into that key.
func (s *Server) handleAssetDownload(
	w http.ResponseWriter, r *http.Request, repositoryName, assetIDValue string,
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
	asset, err := s.logicalStoredAssetByID(r.Context(), repository, assetID)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	s.content.ServeAsset(w, r, asset)
}
