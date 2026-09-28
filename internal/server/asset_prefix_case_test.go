package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestAssetPagePrefixMatchesExactCaseAcrossCursorsAndGroups(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, path := range []string{"Case/A", "Case/B", "case/c"} {
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: "raw", Path: path, Digest: "sha256:" + path,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw-group", Format: "raw", Type: "group", Members: []string{"raw"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"raw", "raw-group"} {
		t.Run(repository, func(t *testing.T) {
			base := "/api/v1/repositories/" + repository + "/assets?prefix=Case/&limit=1"
			cursor := ""
			var paths []string
			for range 4 {
				requestPath := base
				if cursor != "" {
					requestPath += "&cursor=" + url.QueryEscape(cursor)
				}
				response := fixture.request(t, http.MethodGet, requestPath, nil, true)
				assertStatus(t, response, http.StatusOK)
				var page httpx.CollectionPage[domain.Asset]
				if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if len(page.Items) > 1 || page.Total != nil {
					t.Fatalf("invalid bounded page: %+v", page)
				}
				for _, asset := range page.Items {
					paths = append(paths, asset.Path)
				}
				cursor = page.NextCursor
				if cursor == "" {
					break
				}
			}
			if len(paths) != 2 || paths[0] != "Case/A" || paths[1] != "Case/B" || cursor != "" {
				t.Fatalf("prefix results = %v, cursor = %q", paths, cursor)
			}
			search := "/api/v1/search?repository=" + repository + "&pathPrefix=Case/&limit=1"
			cursor = ""
			paths = nil
			for range 5 {
				requestPath := search
				if cursor != "" {
					requestPath += "&cursor=" + url.QueryEscape(cursor)
				}
				response := fixture.request(t, http.MethodGet, requestPath, nil, true)
				assertStatus(t, response, http.StatusOK)
				var page httpx.CollectionPage[repositoryBrowseItem]
				if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				for _, item := range page.Items {
					paths = append(paths, item.Asset.Path)
				}
				cursor = page.NextCursor
				if cursor == "" {
					break
				}
			}
			if len(paths) != 2 || paths[0] != "Case/A" || paths[1] != "Case/B" || cursor != "" {
				t.Fatalf("search prefix results = %v, cursor = %q", paths, cursor)
			}
		})
	}
}
