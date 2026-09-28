package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestIndexedNameAndAssetPathBoundariesAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := strings.Repeat("r", domain.MaxResourceNameBytes)
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: name, Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatalf("create %d-byte repository name: %v", len(name), err)
		}
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: name + "r", Format: "raw", Type: "hosted",
		}); !errors.Is(err, domain.ErrInvalidRepository) {
			t.Fatalf("create overlong repository name: %v", err)
		}
		username := strings.Repeat("u", domain.MaxUsernameBytes)
		if err := metadata.CreateUser(ctx, username, "secret", false); err != nil {
			t.Fatalf("create %d-byte username: %v", len(username), err)
		}
		if err := metadata.CreateUser(ctx, username+"u", "secret", false); !errors.Is(err, domain.ErrInvalidUsername) {
			t.Fatalf("create overlong username: %v", err)
		}
		path := strings.Repeat("p", domain.MaxAssetPathBytes)
		asset := domain.Asset{Repository: name, Path: path, Digest: "sha256:" + strings.Repeat("a", 64)}
		if _, err := metadata.PutAsset(ctx, asset); err != nil {
			t.Fatalf("store %d-byte indexed asset path: %v", len(path), err)
		}
		asset.Path += "p"
		if _, err := metadata.PutAsset(ctx, asset); !errors.Is(err, domain.ErrInvalidAssetPath) {
			t.Fatalf("store overlong asset path: %v", err)
		}
		asset.Path = strings.Repeat("é", domain.MaxAssetPathBytes/2)
		if _, err := metadata.PutAsset(ctx, asset); err != nil {
			t.Fatalf("store valid %d-byte UTF-8 path: %v", len(asset.Path), err)
		}
		asset.Path = "invalid-" + string([]byte{0xff})
		if _, err := metadata.PutAsset(ctx, asset); !errors.Is(err, domain.ErrInvalidAssetPath) {
			t.Fatalf("store invalid UTF-8 path: %v", err)
		}
	})
}
