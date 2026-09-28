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

func TestAssetListingUsesBoundedKeysetPages(t *testing.T) {
	fixture := newServerFixture(t)
	repository, err := fixture.Metadata.Repository(context.Background(), "raw")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
			Repository: "raw", RepositoryID: repository.ID,
			Path: fmt.Sprintf("paged/%02d", i), Digest: "sha256:" + strings.Repeat("a", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string, status int) httpx.CollectionPage[domain.Asset] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, status)
		defer response.Body.Close()
		if status != http.StatusOK {
			return httpx.CollectionPage[domain.Asset]{}
		}
		var page httpx.CollectionPage[domain.Asset]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	base := "/api/v1/repositories/raw/assets?prefix=paged/"
	first := read(base+"&limit=2", http.StatusOK)
	if len(first.Items) != 2 || first.Items[0].Path != "paged/00" || first.Items[1].Path != "paged/01" || first.NextCursor == "" || first.Total != nil {
		t.Fatalf("unexpected first keyset page: %+v", first)
	}
	// The first cursor excludes later inserts, even those sorting before a
	// remaining row. A fresh traversal sees the inserted path.
	_, err = fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "raw", RepositoryID: repository.ID,
		Path: "paged/025", Digest: "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := read(base+"&limit=2&cursor="+url.QueryEscape(first.NextCursor), http.StatusOK)
	if len(second.Items) != 2 || second.Items[0].Path != "paged/02" || second.Items[1].Path != "paged/03" || second.NextCursor == "" || second.Total != nil {
		t.Fatalf("unexpected second keyset page: %+v", second)
	}
	freshFirst := read(base+"&limit=2", http.StatusOK)
	freshSecond := read(base+"&limit=2&cursor="+url.QueryEscape(freshFirst.NextCursor), http.StatusOK)
	if len(freshSecond.Items) != 2 || freshSecond.Items[1].Path != "paged/025" {
		t.Fatalf("fresh traversal omitted new asset: %+v", freshSecond)
	}
	third := read(base+"&limit=2&cursor="+url.QueryEscape(second.NextCursor), http.StatusOK)
	if len(third.Items) != 1 || third.Items[0].Path != "paged/04" || third.NextCursor != "" {
		t.Fatalf("unexpected final keyset page: %+v", third)
	}
	read(base+"&page=1", http.StatusBadRequest)
	read(base+"&page=1&cursor="+url.QueryEscape(first.NextCursor), http.StatusBadRequest)
	read(base+"&limit=0", http.StatusBadRequest)
	read("/api/v1/repositories/raw/assets?prefix=other/&cursor="+url.QueryEscape(first.NextCursor), http.StatusConflict)
	if _, err := fixture.Metadata.DeleteAsset(context.Background(), "raw", "paged/01"); err != nil {
		t.Fatal(err)
	}
	read(base+"&cursor="+url.QueryEscape(first.NextCursor), http.StatusConflict)
}
