package content

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestVisitAssetPathsPagingAndControl(t *testing.T) {
	ctx := context.Background()
	metadata, err := store.OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "default",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_BLOBSTORE",
		},
		PhysicalIdentity: strings.Repeat("0", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "enumeration", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	repository, err := metadata.Repository(ctx, "enumeration")
	if err != nil {
		t.Fatal(err)
	}
	put := func(path string) {
		t.Helper()
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: repository.Name, RepositoryID: repository.ID, Path: path,
			Digest: "sha256:" + strings.Repeat("a", 64),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 260 {
		put(fmt.Sprintf("p/%04d", i))
	}
	put("p/%literal")
	put("p/_literal")
	put("P/%literal")
	view := metadata.ForRepository(repository)
	runtime := New(Options{Metadata: metadata})
	for name, enumerate := range map[string]func(context.Context, string, func(string) (bool, error)) error{
		"stored": (storedAssetsView{runtime: runtime, repository: repository}).VisitAssetPaths,
		"wire":   runtime.WireTools(repository).VisitAssetPaths,
	} {
		var matched []string
		if err := enumerate(ctx, "p/%", func(path string) (bool, error) {
			matched = append(matched, path)
			return true, nil
		}); err != nil || len(matched) != 1 || matched[0] != "p/%literal" {
			t.Fatalf("%s literal prefix: paths=%v err=%v", name, matched, err)
		}
	}

	var got []string
	err = visitAssetPaths(ctx, view, "p/", func(path string) (bool, error) {
		got = append(got, path)
		if path == "p/0126" {
			if _, err := metadata.DeleteAsset(ctx, repository.Name, "p/0126"); err != nil {
				return false, err
			}
			put("p/0126x")
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 263 || !slices.IsSorted(got) || slices.Index(got, "p/0126x") != slices.Index(got, "p/0126")+1 {
		t.Fatalf("unexpected page traversal: count=%d sorted=%v cursor insertion=%d", len(got), slices.IsSorted(got), slices.Index(got, "p/0126x"))
	}

	for _, prefix := range []string{"p/%", "p/_"} {
		var matched []string
		if err := visitAssetPaths(ctx, view, prefix, func(path string) (bool, error) {
			matched = append(matched, path)
			return true, nil
		}); err != nil || len(matched) != 1 || matched[0] != prefix+"literal" {
			t.Fatalf("literal prefix %q: paths=%v err=%v", prefix, matched, err)
		}
	}

	count := 0
	if err := visitAssetPaths(ctx, view, "p/", func(string) (bool, error) {
		count++
		return false, nil
	}); err != nil || count != 1 {
		t.Fatalf("early stop: count=%d err=%v", count, err)
	}
	wantErr := errors.New("visitor failed")
	count = 0
	if err := visitAssetPaths(ctx, view, "p/", func(string) (bool, error) {
		count++
		if count == 3 {
			return false, wantErr
		}
		return true, nil
	}); !errors.Is(err, wantErr) || count != 3 {
		t.Fatalf("callback error: count=%d err=%v", count, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	count = 0
	if err := visitAssetPaths(canceled, view, "p/", func(string) (bool, error) {
		count++
		cancel()
		return true, nil
	}); !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancellation: count=%d err=%v", count, err)
	}
	finalCanceled, cancelFinal := context.WithCancel(ctx)
	count = 0
	if err := visitAssetPaths(finalCanceled, view, "p/%", func(string) (bool, error) {
		count++
		cancelFinal()
		return true, nil
	}); !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("final-page cancellation: count=%d err=%v", count, err)
	}
}
