package content

import (
	"context"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// assetOverrideView is the repository view a metadata wrapper mints to intercept
// a repository-scoped read; it overrides one method and would delegate the rest.
type assetOverrideView struct {
	store.RepositoryView
	called *bool
	asset  domain.Asset
}

func (v assetOverrideView) Asset(context.Context, string) (domain.Asset, error) {
	*v.called = true
	return v.asset, nil
}

// assetOverrideStore is a metadata wrapper of the kind installed through
// SetMetadata: it embeds store.Store and overrides ForRepository to mint a view
// over itself, so repository-scoped reads observe the wrapper rather than the
// raw backend.
type assetOverrideStore struct {
	store.Store
	called *bool
	asset  domain.Asset
}

func (s assetOverrideStore) ForRepository(domain.Repository) store.RepositoryView {
	return assetOverrideView{called: s.called, asset: s.asset}
}

// TestOCIRepositoryViewDelegatesThroughWrapper guards the repository-scoped OCI
// path against binding its view to the raw backend. The view must be minted by
// the runtime's current backend so a SetMetadata wrapper's scoped-read overrides
// are honored; a view bound to an embedded SQLStore would silently bypass them.
func TestOCIRepositoryViewDelegatesThroughWrapper(t *testing.T) {
	called := false
	marker := domain.Asset{Path: "marker"}
	rt := &Runtime{}
	rt.metadata = assetOverrideStore{called: &called, asset: marker}

	view := rt.OCIRepositoryView(domain.Repository{ID: "r1", Name: "repo"})
	got, err := view.Asset(context.Background(), "some/path")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("OCI repository view bypassed the SetMetadata wrapper's ForRepository override")
	}
	if got.Path != marker.Path {
		t.Fatalf("view returned %+v, want the wrapper's asset", got)
	}
}
