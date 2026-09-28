package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestGroupAssetsPagesVisibleMembersAndKeepsSnapshot(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "raw-second", Format: "raw", Type: "hosted"},
		{Name: "raw-group", Format: "raw", Type: "group", Members: []string{"raw", "raw-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	put := func(repository, path, digest string) domain.Asset {
		t.Helper()
		asset, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: repository, Path: path, Digest: digest,
		})
		if err != nil {
			t.Fatal(err)
		}
		return asset
	}
	first := put("raw", "pkg/a", "sha256:first")
	third := put("raw", "pkg/c", "sha256:third")
	shadowed := put("raw-second", "pkg/a", "sha256:shadowed")
	second := put("raw-second", "pkg/b", "sha256:second")
	put("raw-second", "other/hidden", "sha256:other")

	base := "/api/v1/repositories/raw-group/assets?prefix=pkg/&limit=1"
	read := func(path string) httpx.CollectionPage[domain.Asset] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[domain.Asset]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 1 || page.Total != nil {
			t.Fatalf("unbounded asset page: %+v", page)
		}
		return page
	}
	firstPage := read(base)
	if len(firstPage.Items) != 1 || firstPage.Items[0].ID != first.ID || firstPage.NextCursor == "" {
		t.Fatalf("first group asset page = %+v", firstPage)
	}
	// An insertion after the first page must stay outside this traversal.
	put("raw-second", "pkg/d", "sha256:new")
	wrongPrefix := fixture.request(t, http.MethodGet,
		"/api/v1/repositories/raw-group/assets?prefix=other/&cursor="+url.QueryEscape(firstPage.NextCursor), nil, true)
	assertStatus(t, wrongPrefix, http.StatusConflict)
	wrongPrefix.Body.Close()
	wrongRoute := fixture.request(t, http.MethodGet,
		"/api/v1/repositories/raw-group/components?cursor="+url.QueryEscape(firstPage.NextCursor), nil, true)
	assertStatus(t, wrongRoute, http.StatusConflict)
	wrongRoute.Body.Close()
	badPage := fixture.request(t, http.MethodGet, base+"&page=1", nil, true)
	assertStatus(t, badPage, http.StatusBadRequest)
	badPage.Body.Close()

	seen := []domain.Asset{firstPage.Items[0]}
	cursor := firstPage.NextCursor
	for pageNumber := 0; cursor != "" && pageNumber < 8; pageNumber++ {
		page := read(base + "&cursor=" + url.QueryEscape(cursor))
		seen = append(seen, page.Items...)
		cursor = page.NextCursor
	}
	if cursor != "" || len(seen) != 3 || seen[0].ID != first.ID || seen[1].ID != third.ID || seen[2].ID != second.ID {
		t.Fatalf("visible group assets = %+v, cursor = %q", seen, cursor)
	}
	for _, asset := range seen {
		if asset.Repository != "raw-group" || asset.RepositoryID != "" {
			t.Fatalf("physical repository leaked in %+v", asset)
		}
	}
	if seen[0].Digest != "sha256:first" {
		t.Fatalf("shadowed member won path: %+v", seen[0])
	}
	response := fixture.request(t, http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/raw-group/assets/%d", shadowed.ID), nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
}

func TestGroupAssetsBoundsShadowScanAndContinuesAfterEmptyPage(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "raw-second", Format: "raw", Type: "hosted"},
		{Name: "raw-group", Format: "raw", Type: "group", Members: []string{"raw", "raw-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	assets := make([]domain.Asset, 0, 2*discoveryScanBudget+1)
	for _, repository := range []string{"raw", "raw-second"} {
		for index := range discoveryScanBudget {
			assets = append(assets, domain.Asset{
				Repository: repository, Path: fmt.Sprintf("pkg/%04d", index),
				Digest: "sha256:" + strings.Repeat("a", 64),
			})
		}
	}
	assets = append(assets, domain.Asset{
		Repository: "raw-second", Path: "pkg/needle", Digest: "sha256:" + strings.Repeat("b", 64),
	})
	if _, err := fixture.Metadata.PutAssets(ctx, assets); err != nil {
		t.Fatal(err)
	}
	counted := &countedDiscoveryStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(counted)
	base := "/api/v1/repositories/raw-group/assets?limit=200"
	read := func(cursor string) httpx.CollectionPage[domain.Asset] {
		t.Helper()
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[domain.Asset]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	cursor := ""
	for range 5 {
		page := read(cursor)
		if len(page.Items) != 200 || page.NextCursor == "" {
			t.Fatalf("visible member page = %+v", page)
		}
		cursor = page.NextCursor
	}
	counted.calls = 0
	empty := read(cursor)
	if len(empty.Items) != 0 || empty.NextCursor == "" || empty.Total != nil {
		t.Fatalf("shadow-only group page = %+v", empty)
	}
	// One empty query advances past the exhausted first member, followed by
	// five bounded queries against the shadowed second member.
	if counted.calls != 1+discoveryScanBudget/httpx.MaximumCollectionLimit || counted.maximumLimit > httpx.MaximumCollectionLimit {
		t.Fatalf("shadow scan made %d calls with maximum SQL limit %d", counted.calls, counted.maximumLimit)
	}
	last := read(empty.NextCursor)
	if len(last.Items) != 1 || last.Items[0].Path != "pkg/needle" || last.NextCursor != "" {
		t.Fatalf("page after shadow scan = %+v", last)
	}
}
