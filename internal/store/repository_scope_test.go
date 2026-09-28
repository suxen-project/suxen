package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestRepositoryScopeDoesNotReachSameNameReplacementOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		name := fmt.Sprintf("scoped-%d", time.Now().UnixNano())
		definition := domain.Repository{Name: name, Format: "raw", Type: "hosted"}
		if err := metadata.CreateRepository(ctx, definition); err != nil {
			t.Fatal(err)
		}
		old, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		oldScope := forRepository(metadata, old)
		if err := metadata.DeleteRepository(ctx, name, Ownership{}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateRepository(ctx, definition); err != nil {
			t.Fatal(err)
		}
		fresh, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.ID == old.ID {
			t.Fatal("repository ID reused")
		}
		freshAsset, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: name, RepositoryID: fresh.ID, Path: "artifact", Digest: "sha256:" + strings.Repeat("a", 64),
			Attributes: map[string]any{"example": map[string]any{"original": true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := metadata.Asset(ctx, name, "artifact")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := oldScope.Repository(ctx); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale repository read: %v", err)
		}
		if _, err := oldScope.DownloadGate(ctx); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale download gate read: %v", err)
		}
		if _, err := oldScope.EffectiveTrustPolicy(ctx); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale trust policy read: %v", err)
		}
		if _, err := oldScope.Asset(ctx, "artifact"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale read: %v", err)
		}
		if _, err := oldScope.AssetByID(ctx, freshAsset.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale asset ID read: %v", err)
		}
		if assets, err := oldScope.Assets(ctx, ""); err != nil || len(assets) != 0 {
			t.Fatalf("stale list: %+v, %v", assets, err)
		}
		if _, err := oldScope.DeleteAsset(ctx, "artifact"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale delete: %v", err)
		}
		at := time.Now().UTC().Add(time.Hour)
		if err := oldScope.TouchAsset(ctx, freshAsset.ID, at); err != nil {
			t.Fatalf("stale touch: %v", err)
		}
		if err := oldScope.RefreshAsset(ctx, freshAsset.ID, at); err != nil {
			t.Fatalf("stale refresh: %v", err)
		}
		if err := oldScope.SetAttributes(ctx, freshAsset.ID, "example", map[string]any{"changed": true}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale attribute set: %v", err)
		}
		if err := oldScope.DeleteAttributes(ctx, freshAsset.ID, "example"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale attribute delete: %v", err)
		}
		if err := metadata.PutNegativeCacheByRepositoryID(ctx, fresh.ID, "missing", time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if hit, err := oldScope.NegativeCacheHit(ctx, "missing", time.Now()); err != nil || hit {
			t.Fatalf("stale negative cache: %v, %v", hit, err)
		}
		if err := oldScope.ClearNegativeCache(ctx, "missing"); err != nil {
			t.Fatal(err)
		}
		newScope := forRepository(metadata, fresh)
		if hit, err := newScope.NegativeCacheHit(ctx, "missing", time.Now()); err != nil || !hit {
			t.Fatalf("replacement negative cache cleared: %v, %v", hit, err)
		}
		retained, err := newScope.Asset(ctx, "artifact")
		if err != nil {
			t.Fatalf("replacement asset lost: %v", err)
		}
		if retained.LastAccessed == nil || baseline.LastAccessed == nil ||
			!retained.LastAccessed.Equal(*baseline.LastAccessed) ||
			!retained.ValidatedAt.Equal(baseline.ValidatedAt) {
			t.Fatalf("stale view changed replacement asset timestamps: %+v", retained)
		}
		if !reflect.DeepEqual(retained.Attributes, baseline.Attributes) {
			t.Fatalf("stale view changed replacement asset attributes: %+v", retained.Attributes)
		}
	})
}

func TestStaleOCIDigestDeleteCannotRemoveReplacementOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		name := fmt.Sprintf("oci-scope-%d", time.Now().UnixNano())
		definition := domain.Repository{Name: name, Format: "oci", Type: "hosted"}
		if err := metadata.CreateRepository(ctx, definition); err != nil {
			t.Fatal(err)
		}
		old, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteRepository(ctx, name, Ownership{}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateRepository(ctx, definition); err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("b", 64)
		path := "v2/acme/widget/manifests/" + digest
		for _, asset := range []domain.Asset{
			{Repository: name, Path: path, Digest: digest, Kind: "oci-manifest", Reference: digest},
			{Repository: name, Path: "v2/acme/widget/manifests/latest", Digest: digest, Kind: "oci-manifest", Reference: "latest"},
		} {
			if _, err := metadata.PutAsset(ctx, asset); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := forRepository(metadata, old).DeleteOCIManifestByDigest(ctx, path); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale OCI digest delete: %v", err)
		}
		if _, err := metadata.Asset(ctx, name, path); err != nil {
			t.Fatalf("replacement manifest removed: %v", err)
		}
		if _, err := metadata.Asset(ctx, name, "v2/acme/widget/manifests/latest"); err != nil {
			t.Fatalf("replacement tag removed: %v", err)
		}
	})
}
