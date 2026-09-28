package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// Search has two independent work budgets: candidate repository rows and raw
// assets. A selective filter may therefore produce an empty continuation page.
const searchRepositoryScanBudget = 1000

type searchPageCursor struct {
	Version    int    `json:"v"`
	Filter     string `json:"f"`
	AfterRoot  string `json:"a,omitempty"`
	Root       string `json:"r,omitempty"`
	RootID     string `json:"id,omitempty"`
	Resource   string `json:"s,omitempty"`
	Source     int    `json:"i,omitempty"`
	AfterID    int64  `json:"p,omitempty"`
	MaxAssetID int64  `json:"m"`
}

func parseSearchPageCursor(r *http.Request, filterIdentity string) (searchPageCursor, error) {
	cursor := searchPageCursor{Version: 1, Filter: filterIdentity}
	query := r.URL.Query()
	if _, found := query["page"]; found {
		return cursor, errors.New("page is not supported for search; use cursor")
	}
	values, found := query["cursor"]
	if !found {
		return cursor, nil
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > httpx.MaximumCursorLength {
		return cursor, errors.New("cursor length is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(values[0])
	if err != nil || len(decoded) > 1024 || base64.RawURLEncoding.EncodeToString(decoded) != values[0] {
		return cursor, errors.New("cursor encoding is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var supplied searchPageCursor
	if err := decoder.Decode(&supplied); err != nil {
		return cursor, errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cursor, errors.New("cursor payload is invalid")
	}
	if supplied.Version != 1 || supplied.MaxAssetID < 1 || supplied.Source < 0 ||
		(supplied.Root == "" && (supplied.AfterRoot == "" || supplied.RootID != "" || supplied.Resource != "" || supplied.AfterID != 0 || supplied.Source != 0)) ||
		(supplied.Root != "" && (supplied.RootID == "" || supplied.Resource == "" || supplied.AfterID < 1)) {
		return cursor, errors.New("cursor payload is invalid")
	}
	if supplied.Filter != filterIdentity {
		return cursor, &httpx.StaleCursorError{Reason: "cursor belongs to a different search"}
	}
	return supplied, nil
}

func (s *Server) writeSearchPage(w http.ResponseWriter, r *http.Request, filter searchFilter) {
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	filterIdentity := filter.identity()
	cursor, err := parseSearchPageCursor(r, filterIdentity)
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	if cursor.MaxAssetID == 0 {
		cursor.MaxAssetID, err = s.assetWatermark().MaxAssetID(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if cursor.MaxAssetID == 0 {
			httpx.WriteJSON(w, http.StatusOK, httpx.CollectionPage[repositoryBrowseItem]{Items: []repositoryBrowseItem{}})
			return
		}
	}
	user, authenticated := s.identity.Authenticate(r)
	items := make([]repositoryBrowseItem, 0, limit)
	assetScanned, repositoryScanned := 0, 0
	write := func(next *searchPageCursor) {
		page := httpx.CollectionPage[repositoryBrowseItem]{Items: items}
		if next != nil {
			page.NextCursor = httpx.EncodeCursor(*next)
		}
		httpx.WriteJSON(w, http.StatusOK, page)
	}
	readable := func(repository domain.Repository) (bool, error) {
		if len(filter.repositories) != 0 {
			if _, included := filter.repositories[repository.Name]; !included {
				return false, nil
			}
		}
		return s.identity.UserHasPrivilege(r, user, authenticated, "repository:"+repository.Name+":read")
	}
	accept := func(root domain.Repository, asset domain.Asset) (repositoryBrowseItem, bool) {
		asset.Repository = root.Name
		item := browseItems(root, []domain.Asset{asset})[0]
		return item, searchMatches(item, filter)
	}
	// Resume within the only root whose source descriptors a cursor references.
	if cursor.Root != "" {
		root, lookupErr := s.repositoryReads().Repository(r.Context(), cursor.Root)
		if errors.Is(lookupErr, domain.ErrNotFound) {
			httpx.WriteCursorError(w, &httpx.StaleCursorError{Reason: "search repository no longer exists"})
			return
		}
		if lookupErr != nil {
			httpx.WriteResult(w, nil, lookupErr)
			return
		}
		allowed, authErr := readable(root)
		if authErr != nil {
			httpx.WriteResult(w, nil, authErr)
			return
		}
		if root.ID != cursor.RootID || !allowed {
			httpx.WriteCursorError(w, &httpx.StaleCursorError{Reason: "search repository changed or is no longer readable"})
			return
		}
		sources, sourceErr := s.discoverySources(r.Context(), []domain.Repository{root})
		if sourceErr != nil {
			httpx.WriteResult(w, nil, sourceErr)
			return
		}
		if len(sources) == 0 || cursor.Source >= len(sources) ||
			cursor.Resource != discoveryResource("cross-repository-search:"+filterIdentity, sources) {
			httpx.WriteCursorError(w, &httpx.StaleCursorError{Reason: "search repository sources changed"})
			return
		}
		part, next, scanned, scanErr := s.scanDiscoveryWithinBudget(r.Context(), sources,
			discoveryCursor{Version: 1, Resource: cursor.Resource, Source: cursor.Source,
				AfterID: cursor.AfterID, MaxID: cursor.MaxAssetID},
			filter.pathPrefix, limit, discoveryScanBudget, accept)
		if scanErr != nil {
			writeSearchScanError(w, scanErr)
			return
		}
		items = append(items, part...)
		assetScanned += scanned
		if next != nil {
			cursor.Source, cursor.AfterID = next.Source, next.AfterID
			write(&cursor)
			return
		}
		cursor.AfterRoot = root.Name
		cursor.Root, cursor.RootID, cursor.Resource = "", "", ""
		cursor.Source, cursor.AfterID = 0, 0
		repositoryScanned++
		if len(items) == limit || assetScanned == discoveryScanBudget {
			later, moreErr := s.searchHasLaterRepository(r, filter, cursor.AfterRoot)
			if moreErr != nil {
				httpx.WriteResult(w, nil, moreErr)
				return
			}
			if later {
				write(&cursor)
			} else {
				write(nil)
			}
			return
		}
	}

	after := cursor.AfterRoot
	for repositoryScanned < searchRepositoryScanBudget && assetScanned < discoveryScanBudget {
		batchSize := min(httpx.MaximumCollectionLimit, searchRepositoryScanBudget-repositoryScanned)
		page, pageErr := s.searchRepositoryPage(r, filter, after, batchSize)
		if pageErr != nil {
			httpx.WriteResult(w, nil, pageErr)
			return
		}
		for index, root := range page.Items {
			repositoryScanned++
			after = root.Name
			allowed, authErr := readable(root)
			if authErr != nil {
				httpx.WriteResult(w, nil, authErr)
				return
			}
			if allowed {
				sources, sourceErr := s.discoverySources(r.Context(), []domain.Repository{root})
				if sourceErr != nil {
					httpx.WriteResult(w, nil, sourceErr)
					return
				}
				if len(sources) > 0 {
					resource := discoveryResource("cross-repository-search:"+filterIdentity, sources)
					part, next, scanned, scanErr := s.scanDiscoveryWithinBudget(r.Context(), sources,
						discoveryCursor{Version: 1, Resource: resource, MaxID: cursor.MaxAssetID},
						filter.pathPrefix, limit-len(items), discoveryScanBudget-assetScanned, accept)
					if scanErr != nil {
						writeSearchScanError(w, scanErr)
						return
					}
					items = append(items, part...)
					assetScanned += scanned
					if next != nil {
						cursor.Root, cursor.RootID, cursor.Resource = root.Name, root.ID, resource
						cursor.Source, cursor.AfterID = next.Source, next.AfterID
						write(&cursor)
						return
					}
				}
			}
			more := index+1 < len(page.Items) || page.HasMore
			if len(items) == limit || assetScanned == discoveryScanBudget ||
				repositoryScanned == searchRepositoryScanBudget {
				cursor.AfterRoot = after
				if more {
					write(&cursor)
				} else {
					write(nil)
				}
				return
			}
		}
		if !page.HasMore {
			write(nil)
			return
		}
	}
	write(nil)
}

func (s *Server) searchHasLaterRepository(r *http.Request, filter searchFilter, after string) (bool, error) {
	page, err := s.searchRepositoryPage(r, filter, after, 1)
	return len(page.Items) > 0, err
}

// A single named repository is common for filtered searches. Resolve it
// directly instead of walking every preceding repository name.
func (s *Server) searchRepositoryPage(r *http.Request, filter searchFilter, after string, limit int) (store.RepositoryKeysetPage, error) {
	if len(filter.repositories) != 1 {
		return s.repositoryReads().RepositoriesPage(r.Context(), after, limit)
	}
	page := store.RepositoryKeysetPage{Items: []domain.Repository{}}
	for name := range filter.repositories {
		if name <= after {
			return page, nil
		}
		repository, err := s.repositoryReads().Repository(r.Context(), name)
		if errors.Is(err, domain.ErrNotFound) {
			return page, nil
		}
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, repository)
	}
	return page, nil
}

func writeSearchScanError(w http.ResponseWriter, err error) {
	var stale *httpx.StaleCursorError
	if errors.As(err, &stale) {
		httpx.WriteCursorError(w, stale)
	} else {
		httpx.WriteResult(w, nil, err)
	}
}
