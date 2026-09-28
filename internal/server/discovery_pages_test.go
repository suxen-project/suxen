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
	"github.com/suxen-project/suxen/internal/store"
)

type countedDiscoveryStore struct {
	store.Store
	calls        int
	maximumLimit int
}

func (metadata *countedDiscoveryStore) ForRepository(repository domain.Repository) store.RepositoryView {
	return &countedDiscoveryView{RepositoryView: metadata.Store.ForRepository(repository), store: metadata}
}

type countedDiscoveryView struct {
	store.RepositoryView
	store *countedDiscoveryStore
}

func (view *countedDiscoveryView) AssetPage(ctx context.Context, request store.AssetPageRequest) (store.AssetPage, error) {
	view.store.calls++
	view.store.maximumLimit = max(view.store.maximumLimit, request.Limit)
	return view.RepositoryView.AssetPage(ctx, request)
}

func TestSearchPagesBoundScannedAssetsEvenWhenFilterIsSelective(t *testing.T) {
	fixture := newServerFixture(t)
	repository, err := fixture.Metadata.Repository(context.Background(), "raw")
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]domain.Asset, 0, discoveryScanBudget+6)
	for i := 0; i < discoveryScanBudget+5; i++ {
		assets = append(assets, domain.Asset{
			Repository: "raw", RepositoryID: repository.ID,
			Path: fmt.Sprintf("noise/%04d", i), Digest: "sha256:" + strings.Repeat("a", 64),
		})
	}
	assets = append(assets, domain.Asset{
		Repository: "raw", RepositoryID: repository.ID,
		Path: "noise/needle", Digest: "sha256:" + strings.Repeat("b", 64),
	})
	if _, err := fixture.Metadata.PutAssets(context.Background(), assets); err != nil {
		t.Fatal(err)
	}
	counted := &countedDiscoveryStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(counted)
	request := "/api/v1/search?repository=raw&q=needle&limit=2"
	read := func(path string) httpx.CollectionPage[repositoryBrowseItem] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[repositoryBrowseItem]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read(request)
	if len(first.Items) != 0 || first.NextCursor == "" || first.Total != nil {
		t.Fatalf("first selective page = %+v, want empty continuation", first)
	}
	if counted.maximumLimit > httpx.MaximumCollectionLimit || counted.calls != discoveryScanBudget/httpx.MaximumCollectionLimit {
		t.Fatalf("first page store calls = %d with max limit %d", counted.calls, counted.maximumLimit)
	}
	second := read(request + "&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 1 || second.Items[0].Asset.Path != "noise/needle" || second.NextCursor != "" {
		t.Fatalf("second selective page = %+v, want needle", second)
	}
	if second.Total != nil {
		t.Fatal("search must not count the full result set")
	}
	response := fixture.request(t, http.MethodGet, request+"&page=1", nil, true)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}

type countedRepositoryPageStore struct {
	store.Store
	calls        int
	maximumLimit int
}

type syntheticSearchRepositoryStore struct {
	store.Store
	repositories []domain.Repository
	pageCalls    int
	maxLimit     int
	fullCalls    int
	assetCalls   int
}

func (metadata *syntheticSearchRepositoryStore) Repositories(context.Context) ([]domain.Repository, error) {
	metadata.fullCalls++
	return nil, fmt.Errorf("unbounded repository listing is forbidden in search")
}

func (metadata *syntheticSearchRepositoryStore) RepositoriesPage(_ context.Context, after string, limit int) (store.RepositoryKeysetPage, error) {
	metadata.pageCalls++
	metadata.maxLimit = max(metadata.maxLimit, limit)
	start := 0
	for start < len(metadata.repositories) && metadata.repositories[start].Name <= after {
		start++
	}
	end := min(start+limit, len(metadata.repositories))
	return store.RepositoryKeysetPage{
		Items: metadata.repositories[start:end], HasMore: end < len(metadata.repositories),
	}, nil
}

func (metadata *syntheticSearchRepositoryStore) MaxAssetID(context.Context) (int64, error) {
	return 1, nil
}

func (metadata *syntheticSearchRepositoryStore) ForRepository(domain.Repository) store.RepositoryView {
	return &syntheticSearchRepositoryView{store: metadata}
}

type syntheticSearchRepositoryView struct {
	store.RepositoryView
	store *syntheticSearchRepositoryStore
}

func (view *syntheticSearchRepositoryView) AssetPage(_ context.Context, request store.AssetPageRequest) (store.AssetPage, error) {
	view.store.assetCalls++
	if request.Limit > httpx.MaximumCollectionLimit {
		return store.AssetPage{}, fmt.Errorf("unbounded asset page limit %d", request.Limit)
	}
	return store.AssetPage{Items: []domain.Asset{}}, nil
}

func TestSearchBoundsRepositoryMetadataScan(t *testing.T) {
	fixture := newServerFixture(t)
	const repositoryCount = searchRepositoryScanBudget + 7
	virtual := make([]domain.Repository, repositoryCount)
	for index := range virtual {
		virtual[index] = domain.Repository{
			ID:   fmt.Sprintf("synthetic-%04d", index),
			Name: fmt.Sprintf("repo-%04d", index), Format: "raw", Type: "hosted",
		}
	}
	counted := &syntheticSearchRepositoryStore{Store: fixture.Metadata, repositories: virtual}
	fixture.Handler.setMetadata(counted)
	read := func(path string) httpx.CollectionPage[repositoryBrowseItem] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[repositoryBrowseItem]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("/api/v1/search?q=missing&limit=10")
	if len(first.Items) != 0 || first.NextCursor == "" {
		t.Fatalf("first metadata-budget page = %+v", first)
	}
	if counted.fullCalls != 0 || counted.pageCalls != 5 || counted.maxLimit > httpx.MaximumCollectionLimit ||
		counted.assetCalls != searchRepositoryScanBudget {
		t.Fatalf("repository scan: full=%d pages=%d max limit=%d asset calls=%d",
			counted.fullCalls, counted.pageCalls, counted.maxLimit, counted.assetCalls)
	}
	second := read("/api/v1/search?q=missing&limit=10&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 0 || second.NextCursor != "" || counted.assetCalls != repositoryCount {
		t.Fatalf("second metadata-budget page = %+v; scanned %d roots", second, counted.assetCalls)
	}
	wrongFilter := fixture.request(t, http.MethodGet,
		"/api/v1/search?q=changed&cursor="+url.QueryEscape(first.NextCursor), nil, true)
	assertStatus(t, wrongFilter, http.StatusConflict)
	wrongFilter.Body.Close()
}

func TestSearchCombinesRepositoriesAndResumesWithinOneRoot(t *testing.T) {
	fixture := newServerFixture(t)
	for _, asset := range []domain.Asset{
		{Repository: "oci", Path: "match/a", Digest: "sha256:a"},
		{Repository: "oci", Path: "match/b", Digest: "sha256:b"},
		{Repository: "oci", Path: "match/c", Digest: "sha256:c"},
		{Repository: "raw", Path: "match/d", Digest: "sha256:d"},
	} {
		if _, err := fixture.Metadata.PutAsset(context.Background(), asset); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) httpx.CollectionPage[repositoryBrowseItem] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[repositoryBrowseItem]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	base := "/api/v1/search?pathPrefix=match%2F&limit=2"
	first := read(base)
	if len(first.Items) != 2 || first.Items[0].Asset.Path != "match/a" ||
		first.Items[1].Asset.Path != "match/b" || first.NextCursor == "" {
		t.Fatalf("first search page = %+v", first)
	}
	second := read(base + "&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 2 || second.Items[0].Asset.Path != "match/c" ||
		second.Items[1].Asset.Path != "match/d" || second.NextCursor != "" {
		t.Fatalf("second search page = %+v", second)
	}
	if err := fixture.Metadata.DeleteRepository(context.Background(), "oci", store.Ownership{Force: true}); err != nil {
		t.Fatal(err)
	}
	stale := fixture.request(t, http.MethodGet, base+"&cursor="+url.QueryEscape(first.NextCursor), nil, true)
	assertStatus(t, stale, http.StatusConflict)
	stale.Body.Close()
}

func TestSearchSingleRepositoryFilterUsesDirectLookup(t *testing.T) {
	fixture := newServerFixture(t)
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "raw", Path: "match/one", Digest: "sha256:one",
	}); err != nil {
		t.Fatal(err)
	}
	counted := &countedRepositoryPageStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(counted)
	response := fixture.request(t, http.MethodGet,
		"/api/v1/search?repository=raw&pathPrefix=match%2F", nil, true)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 1 || page.NextCursor != "" || counted.calls != 0 {
		t.Fatalf("direct-filter search = %+v, repository page calls = %d", page, counted.calls)
	}
}

func (metadata *countedRepositoryPageStore) RepositoriesPage(ctx context.Context, after string, limit int) (store.RepositoryKeysetPage, error) {
	metadata.calls++
	metadata.maximumLimit = max(metadata.maximumLimit, limit)
	return metadata.Store.RepositoriesPage(ctx, after, limit)
}

func TestBrowseRepositoriesUsesBoundedNameCursor(t *testing.T) {
	fixture := newServerFixture(t)
	counted := &countedRepositoryPageStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(counted)
	read := func(path string) httpx.CollectionPage[browseRepositoryDescriptor] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[browseRepositoryDescriptor]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("/api/v1/browse?limit=1")
	if len(first.Items) != 1 || first.NextCursor == "" || first.Total != nil {
		t.Fatalf("first repository discovery page = %+v", first)
	}
	second := read("/api/v1/browse?limit=1&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 1 || second.Items[0].Name <= first.Items[0].Name || second.Total != nil {
		t.Fatalf("second repository discovery page = %+v", second)
	}
	if counted.calls < 2 || counted.maximumLimit > httpx.MaximumCollectionLimit {
		t.Fatalf("repository page calls = %d, max limit = %d", counted.calls, counted.maximumLimit)
	}
	response := fixture.request(t, http.MethodGet, "/api/v1/browse?page=1", nil, true)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}

func TestGroupBrowseCursorKeepsFirstMemberPrecedence(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "priority-first", Format: "raw", Type: "hosted"},
		{Name: "priority-second", Format: "raw", Type: "hosted"},
		{Name: "priority-group", Format: "raw", Type: "group", Members: []string{"priority-first", "priority-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	for _, asset := range []domain.Asset{
		{Repository: "priority-first", Path: "a", Digest: "sha256:a"},
		{Repository: "priority-first", Path: "shared", Digest: "sha256:first"},
		{Repository: "priority-second", Path: "shared", Digest: "sha256:second"},
		{Repository: "priority-second", Path: "z", Digest: "sha256:z"},
	} {
		if _, err := fixture.Metadata.PutAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/repositories/priority-group/browse?limit=1"
	cursor := ""
	var paths []string
	var digests []string
	for {
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		var page httpx.CollectionPage[repositoryBrowseItem]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		for _, item := range page.Items {
			if item.Asset.Repository != "priority-group" {
				t.Fatalf("member name leaked: %+v", item.Asset)
			}
			paths = append(paths, item.Asset.Path)
			digests = append(digests, item.Asset.Digest)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if len(paths) > 4 {
			t.Fatal("browse cursor did not terminate")
		}
	}
	if fmt.Sprint(paths) != "[a shared z]" || fmt.Sprint(digests) != "[sha256:a sha256:first sha256:z]" {
		t.Fatalf("group browse paths = %v, digests = %v", paths, digests)
	}
}
