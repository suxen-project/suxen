package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// Recreate at the repository lookup boundary: the handler has the old value,
// while every subsequent metadata operation observes the replacement.
type recreateOnLookupStore struct {
	store.Store
	once          sync.Once
	mutate        func() error
	mutationError error
}

func (metadata *recreateOnLookupStore) Repository(ctx context.Context, name string) (domain.Repository, error) {
	repository, err := metadata.Store.Repository(ctx, name)
	if err == nil && name == "mirror" {
		metadata.once.Do(func() { metadata.mutationError = metadata.mutate() })
		if metadata.mutationError != nil {
			return domain.Repository{}, metadata.mutationError
		}
	}
	return repository, err
}

func TestResolvedRawRequestCannotReadOrDeleteReplacement(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			fixture := newServerFixture(t)
			ctx := context.Background()
			if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "mirror", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			seed := fixture.request(t, http.MethodPut, "/repository/raw/replacement", []byte("replacement bytes"), true)
			assertStatus(t, seed, http.StatusCreated)
			seed.Body.Close()
			staged, err := fixture.Metadata.Asset(ctx, "raw", "replacement")
			if err != nil {
				t.Fatal(err)
			}
			old, err := fixture.Metadata.Repository(ctx, "mirror")
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &recreateOnLookupStore{Store: fixture.Metadata}
			wrapped.mutate = func() error {
				if err := fixture.Metadata.DeleteRepository(ctx, "mirror", store.Ownership{Force: true}); err != nil {
					return err
				}
				if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
					Name: "mirror", Format: "raw", Type: "proxy", Upstream: "https://origin.example.test/raw",
				}); err != nil {
					return err
				}
				_, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
					Repository: "mirror", Path: "file", Digest: staged.Digest,
					Size: staged.Size, ContentType: "application/octet-stream", BlobStore: staged.BlobStore,
				})
				return err
			}
			fixture.Handler.metadata = wrapped
			fixture.Handler.content.SetMetadata(wrapped)
			response := fixture.request(t, method, "/repository/mirror/file", nil, true)
			assertStatus(t, response, http.StatusNotFound)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), old.ID) {
				t.Fatal("internal repository ID leaked in response")
			}
			fresh, err := fixture.Metadata.Repository(ctx, "mirror")
			if err != nil {
				t.Fatal(err)
			}
			if fresh.ID == old.ID {
				t.Fatal("repository identity reused")
			}
			if _, err := fixture.Metadata.Asset(ctx, "mirror", "file"); err != nil {
				t.Fatalf("replacement asset was removed: %v", err)
			}
			newRequest := fixture.request(t, http.MethodGet, "/repository/mirror/file", nil, true)
			assertStatus(t, newRequest, http.StatusOK)
			assertBody(t, newRequest, []byte("replacement bytes"))
		})
	}
}

func TestRetainedWireToolsCannotReadSameNameReplacement(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "mirror", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	old, err := fixture.Metadata.Repository(ctx, "mirror")
	if err != nil {
		t.Fatal(err)
	}
	tools := fixture.Handler.content.WireTools(old)
	if err := fixture.Metadata.DeleteRepository(ctx, "mirror", store.Ownership{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "mirror", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{Repository: "mirror", Path: "new", Digest: "sha256:" + strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	err = tools.VisitAssetPaths(ctx, "", func(path string) (bool, error) {
		paths = append(paths, path)
		return true, nil
	})
	if err != nil || len(paths) != 0 {
		t.Fatalf("stale list: %v, %v", paths, err)
	}
	if _, found, err := tools.StatAsset(ctx, "new"); err != nil || found {
		t.Fatalf("stale stat: %v, %v", found, err)
	}
	reader, _, found, err := tools.OpenAsset(ctx, "new")
	if reader != nil {
		reader.Close()
	}
	if err != nil || found {
		t.Fatalf("stale open: %v, %v", found, err)
	}
	fresh, err := fixture.Metadata.Repository(ctx, "mirror")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := fixture.Handler.content.WireTools(fresh).StatAsset(ctx, "new"); err != nil || !found {
		t.Fatalf("fresh handle cannot stat replacement: %v, %v", found, err)
	}
	if _, err := fixture.Metadata.Asset(ctx, "mirror", "new"); errors.Is(err, domain.ErrNotFound) {
		t.Fatal("replacement asset disappeared")
	}
}
