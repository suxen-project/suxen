package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// A discovery request examines at most this many raw assets. Selective filters
// may yield an empty page with a continuation cursor; consumers must follow it.
const discoveryScanBudget = 1000

// assetWatermark narrows the metadata store to the highest asset ID currently
// persisted. The search and discovery paginators read it once to freeze a
// snapshot ceiling, so a table that keeps growing mid-scan yields a stable page
// set rather than sweeping in newly written assets. It reads s.metadata on each
// call so a reconfigured backend is honoured.
type assetWatermark interface {
	MaxAssetID(context.Context) (int64, error)
}

func (s *Server) assetWatermark() assetWatermark {
	return s.metadata
}

// priorMemberAssets narrows the metadata store to looking up an asset by path
// within a repository addressed only by its ID. Group browse and discovery use
// it to detect whether a member's asset is shadowed by a higher-precedence prior
// member: the callers hold the prior member's ID from the group's ordered member
// list, not a resolved repository, so store.ForRepository (which is for requests
// that have already resolved a repository) does not apply. It reads s.metadata on
// each call so a reconfigured backend is honoured.
type priorMemberAssets interface {
	AssetByRepositoryID(context.Context, string, string) (domain.Asset, error)
	AssetByPublicPath(context.Context, string, string, int64) (domain.Asset, error)
}

func (s *Server) priorMemberAssets() priorMemberAssets {
	return s.metadata
}

type discoverySource struct {
	root     domain.Repository
	leaf     domain.Repository
	priorIDs []string
}

type discoveryCursor struct {
	Version  int    `json:"v"`
	Resource string `json:"r"`
	Source   int    `json:"i"`
	AfterID  int64  `json:"a"`
	MaxID    int64  `json:"m"`
}

func discoveryResource(kind string, sources []discoverySource) string {
	hash := sha256.New()
	hash.Write([]byte(kind))
	for _, source := range sources {
		hash.Write([]byte{0})
		hash.Write([]byte(source.root.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(source.root.Format))
		hash.Write([]byte{0})
		hash.Write([]byte(source.leaf.ID))
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func parseDiscoveryCursor(r *http.Request, resource string, sourceCount int) (discoveryCursor, error) {
	cursor := discoveryCursor{Version: 1, Resource: resource}
	query := r.URL.Query()
	if _, hasPage := query["page"]; hasPage {
		return cursor, errors.New("page is not supported for discovery; use cursor")
	}
	values, found := query["cursor"]
	if !found {
		return cursor, nil
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > httpx.MaximumCursorLength {
		return cursor, errors.New("cursor length is invalid")
	}
	encoded := values[0]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) > 1024 || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return cursor, errors.New("cursor encoding is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var supplied discoveryCursor
	if err := decoder.Decode(&supplied); err != nil {
		return cursor, errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cursor, errors.New("cursor payload is invalid")
	}
	if supplied.Version != 1 || supplied.AfterID < 1 || supplied.MaxID < supplied.AfterID {
		return cursor, errors.New("cursor payload is invalid")
	}
	if supplied.Resource != resource || supplied.Source < 0 || supplied.Source >= sourceCount {
		return cursor, &httpx.StaleCursorError{Reason: "cursor belongs to a different discovery collection"}
	}
	return supplied, nil
}

func (s *Server) discoverySources(ctx context.Context, roots []domain.Repository) ([]discoverySource, error) {
	sources := make([]discoverySource, 0, len(roots))
	for _, root := range roots {
		leaves, err := s.logicalLeaves(ctx, root, make(map[string]bool))
		if err != nil {
			return nil, err
		}
		priorIDs := make([]string, 0, len(leaves))
		for _, leaf := range leaves {
			// Each source holds a prefix view of this root's shared ID slice.
			// Copying the prefix for every member would use quadratic memory.
			sources = append(sources, discoverySource{
				root: root, leaf: leaf, priorIDs: priorIDs,
			})
			priorIDs = append(priorIDs, leaf.ID)
		}
	}
	return sources, nil
}

func (s *Server) logicalLeaves(ctx context.Context, repository domain.Repository, visited map[string]bool) ([]domain.Repository, error) {
	if visited[repository.ID] {
		return nil, nil
	}
	visited[repository.ID] = true
	defer delete(visited, repository.ID)
	if repository.Type != "group" {
		return []domain.Repository{repository}, nil
	}
	leaves := make([]domain.Repository, 0, len(repository.Members))
	for _, name := range repository.Members {
		member, err := s.repositoryReads().Repository(ctx, name)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if member.Format != repository.Format {
			continue
		}
		nested, err := s.logicalLeaves(ctx, member, visited)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, nested...)
	}
	return leaves, nil
}

// scanDiscovery examines at most discoveryScanBudget rows and projects only
// emitted matches. The cursor tracks the last examined row, not the last match.
func (s *Server) scanDiscovery(
	ctx context.Context, sources []discoverySource, cursor discoveryCursor,
	prefix string, limit int, accept func(domain.Repository, domain.Asset) (repositoryBrowseItem, bool),
) ([]repositoryBrowseItem, *discoveryCursor, error) {
	items, next, _, err := s.scanDiscoveryWithinBudget(
		ctx, sources, cursor, prefix, limit, discoveryScanBudget, accept,
	)
	return items, next, err
}

func (s *Server) scanDiscoveryWithinBudget(
	ctx context.Context, sources []discoverySource, cursor discoveryCursor,
	prefix string, limit, budget int,
	accept func(domain.Repository, domain.Asset) (repositoryBrowseItem, bool),
) ([]repositoryBrowseItem, *discoveryCursor, int, error) {
	items := make([]repositoryBrowseItem, 0, limit)
	if len(sources) == 0 {
		return items, nil, 0, nil
	}
	if cursor.MaxID == 0 {
		maximum, err := s.assetWatermark().MaxAssetID(ctx)
		if err != nil {
			return nil, nil, 0, err
		}
		if maximum == 0 {
			return items, nil, 0, nil
		}
		cursor.MaxID = maximum
	}
	scanned := 0
	for index := cursor.Source; index < len(sources) && scanned < budget; index++ {
		source := sources[index]
		afterID := int64(0)
		if index == cursor.Source {
			afterID = cursor.AfterID
		}
		for scanned < budget {
			batchSize := min(httpx.MaximumCollectionLimit, budget-scanned)
			if batchSize < 1 {
				break
			}
			page, err := s.metadata.ForRepository(source.leaf).AssetPage(ctx, store.AssetPageRequest{
				Prefix: prefix, AfterID: afterID, MaxID: cursor.MaxID, Limit: batchSize,
				PublicPrefix: source.root.Type == "group" && source.root.Format == "npm",
			})
			if errors.Is(err, domain.ErrNotFound) && afterID > 0 {
				return nil, nil, scanned, &httpx.StaleCursorError{Reason: "discovery cursor anchor no longer exists"}
			}
			if err != nil {
				return nil, nil, scanned, err
			}
			for rowIndex, asset := range page.Items {
				scanned++
				afterID = asset.ID
				duplicate, err := s.shadowedDiscoveryAsset(ctx, source, asset, cursor.MaxID)
				if err != nil {
					return nil, nil, scanned, err
				}
				if !duplicate {
					if item, ok := accept(source.root, asset); ok {
						items = append(items, item)
					}
				}
				hasMore := rowIndex+1 < len(page.Items) || page.HasMore || index+1 < len(sources)
				if len(items) == limit || scanned == budget {
					if hasMore {
						next := cursor
						next.Source = index
						next.AfterID = afterID
						return items, &next, scanned, nil
					}
					return items, nil, scanned, nil
				}
			}
			if !page.HasMore {
				break
			}
			// A full batch with more rows continues from its last returned ID.
		}
	}
	return items, nil, scanned, nil
}

func (s *Server) shadowedDiscoveryAsset(ctx context.Context, source discoverySource, asset domain.Asset, maxID int64) (bool, error) {
	if visible, decided, err := s.indexedGroupArtifactVisibility(ctx, source.root, source.leaf.Name, asset); err != nil {
		return false, err
	} else if decided {
		return !visible, nil
	}
	publicNpm := source.root.Type == "group" && source.root.Format == "npm"
	path := asset.Path
	if publicNpm {
		path = assetPublicPath(asset)
	}
	for _, repositoryID := range source.priorIDs {
		var prior domain.Asset
		var err error
		if publicNpm {
			prior, err = s.priorMemberAssets().AssetByPublicPath(ctx, repositoryID, path, maxID)
		} else {
			prior, err = s.priorMemberAssets().AssetByRepositoryID(ctx, repositoryID, path)
		}
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if prior.ID <= maxID {
			return true, nil
		}
	}
	return false, nil
}

func assetPublicPath(asset domain.Asset) string {
	if asset.FormatPath != "" {
		return asset.FormatPath
	}
	return asset.Path
}

func (s *Server) writeDiscoveryPage(
	w http.ResponseWriter, r *http.Request, sources []discoverySource, prefix, kind string,
	accept func(domain.Repository, domain.Asset) (repositoryBrowseItem, bool),
) {
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	resource := discoveryResource(kind, sources)
	cursor, err := parseDiscoveryCursor(r, resource, len(sources))
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	items, next, err := s.scanDiscovery(r.Context(), sources, cursor, prefix, limit, accept)
	if err != nil {
		var stale *httpx.StaleCursorError
		if errors.As(err, &stale) {
			httpx.WriteCursorError(w, stale)
		} else {
			httpx.WriteResult(w, nil, err)
		}
		return
	}
	page := httpx.CollectionPage[repositoryBrowseItem]{Items: items}
	if next != nil {
		page.NextCursor = httpx.EncodeCursor(*next)
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}
