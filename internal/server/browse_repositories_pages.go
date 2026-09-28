package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/suxen-project/suxen/internal/httpx"
)

type browseRepositoriesCursor struct {
	Version int    `json:"v"`
	After   string `json:"a"`
}

func parseBrowseRepositoriesCursor(r *http.Request) (browseRepositoriesCursor, error) {
	cursor := browseRepositoriesCursor{Version: 1}
	query := r.URL.Query()
	if _, found := query["page"]; found {
		return cursor, errors.New("page is not supported for repository discovery; use cursor")
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
	if err := decoder.Decode(&cursor); err != nil {
		return cursor, errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cursor, errors.New("cursor payload is invalid")
	}
	if cursor.Version != 1 || cursor.After == "" {
		return cursor, errors.New("cursor payload is invalid")
	}
	return cursor, nil
}

func (s *Server) writeBrowseRepositoriesPage(w http.ResponseWriter, r *http.Request) {
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	cursor, err := parseBrowseRepositoriesCursor(r)
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	user, authenticated := s.identity.Authenticate(r)
	items := make([]browseRepositoryDescriptor, 0, limit)
	scanned := 0
	after := cursor.After
	for scanned < discoveryScanBudget {
		batchSize := min(httpx.MaximumCollectionLimit, discoveryScanBudget-scanned)
		page, err := s.repositoryReads().RepositoriesPage(r.Context(), after, batchSize)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index, repository := range page.Items {
			scanned++
			after = repository.Name
			allowed, err := s.identity.UserHasPrivilege(r, user, authenticated, "repository:"+repository.Name+":read")
			if err != nil {
				httpx.WriteResult(w, nil, err)
				return
			}
			if allowed {
				items = append(items, browseRepositoryDescriptor{
					Name: repository.Name, Format: repository.Format, Type: repository.Type,
				})
			}
			hasMore := index+1 < len(page.Items) || page.HasMore
			if len(items) == limit || scanned == discoveryScanBudget {
				result := httpx.CollectionPage[browseRepositoryDescriptor]{Items: items}
				if hasMore {
					result.NextCursor = httpx.EncodeCursor(browseRepositoriesCursor{Version: 1, After: after})
				}
				httpx.WriteJSON(w, http.StatusOK, result)
				return
			}
		}
		if !page.HasMore {
			break
		}
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.CollectionPage[browseRepositoryDescriptor]{Items: items})
}
