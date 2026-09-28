package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type replacingAssetStore struct {
	store.Store
	once    sync.Once
	replace func()
}

func (s *replacingAssetStore) ForRepository(repository domain.Repository) store.RepositoryView {
	return replacingAssetView{RepositoryView: s.Store.ForRepository(repository), owner: s}
}

type replacingAssetView struct {
	store.RepositoryView
	owner *replacingAssetStore
}

func (v replacingAssetView) AssetByID(ctx context.Context, id int64) (domain.Asset, error) {
	asset, err := v.RepositoryView.AssetByID(ctx, id)
	if err == nil {
		v.owner.once.Do(v.owner.replace)
	}
	return asset, err
}

func TestDeleteAssetByIDPreservesReplacement(t *testing.T) {
	f := newServerFixture(t)
	ctx := context.Background()
	old, err := f.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "raw", Path: "artifact", Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &replacingAssetStore{Store: f.Metadata}
	var replacement domain.Asset
	wrapper.replace = func() {
		replacement, err = f.Metadata.PutAsset(ctx, domain.Asset{
			Repository: "raw", Path: "artifact", Kind: "raw", Digest: "sha256:" + strings.Repeat("b", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.Handler.metadata = wrapper
	response := f.request(t, http.MethodDelete, fmt.Sprintf("/api/v1/repositories/raw/assets/%d", old.ID), nil, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("delete stale ID status = %d, want 404", response.StatusCode)
	}
	if replacement.ID == 0 || replacement.ID == old.ID {
		t.Fatalf("replacement ID = %d, old ID = %d", replacement.ID, old.ID)
	}
	retained, err := f.Metadata.Asset(ctx, "raw", "artifact")
	if err != nil || retained.ID != replacement.ID {
		t.Fatalf("replacement after delete = %+v, %v", retained, err)
	}
	if _, err := f.Metadata.AssetByID(ctx, "raw", old.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old generation still exists: %v", err)
	}
}
