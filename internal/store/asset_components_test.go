package store

import (
	"context"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
)

func componentTestConfig() map[string]any {
	return map[string]any{"components": []any{map[string]any{
		"pattern": `^(?P<name>models/.+)/(?P<version>[0-9][^/]*)/[^/]+$`,
		"anchor":  `\.glb$`,
	}}}
}

func putComponentTestAsset(t *testing.T, metadata *SQLStore, repository, path, kind, reference string) domain.Asset {
	t.Helper()
	asset, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: repository, Path: path, Kind: kind, Reference: reference,
		Digest: "sha256:" + strings.Repeat("c", 64), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func assetComponent(t *testing.T, metadata *SQLStore, repository, path string) (string, string) {
	t.Helper()
	asset, err := metadata.Asset(context.Background(), repository, path)
	if err != nil {
		t.Fatal(err)
	}
	return asset.Component, asset.ComponentVersion
}

func TestAssetComponentMigrationBackfillsRawAndOCIRows(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		for _, repository := range []domain.Repository{
			{Name: "models", Format: "raw", Type: "hosted", FormatConfig: componentTestConfig()},
			{Name: "plain", Format: "raw", Type: "hosted"},
			{Name: "images", Format: "oci", Type: "hosted"},
		} {
			if err := metadata.CreateRepository(ctx, repository); err != nil {
				t.Fatal(err)
			}
		}
		putComponentTestAsset(t, metadata, "models", "models/core/0.2.0/core.glb", "raw", "")
		putComponentTestAsset(t, metadata, "models", "models/core/0.2.0/SHA256SUMS", "raw", "")
		putComponentTestAsset(t, metadata, "models", "docs/readme.txt", "raw", "")
		putComponentTestAsset(t, metadata, "plain", "models/core/0.2.0/core.glb", "raw", "")
		putComponentTestAsset(t, metadata, "images", "v2/team/app/manifests/1.2.3", "oci-manifest", "1.2.3")
		putComponentTestAsset(t, metadata, "images", "v2/team/app/manifests/sha256:"+strings.Repeat("d", 64), "oci-manifest", "sha256:"+strings.Repeat("d", 64))

		// Recreate the pre-16 shape with the rows above, then migrate forward.
		for _, statement := range []string{
			`DELETE FROM schema_migrations WHERE version >= 16`,
			`DROP INDEX idx_assets_component`,
			`ALTER TABLE assets DROP COLUMN component`,
			`ALTER TABLE assets DROP COLUMN component_version`,
		} {
			if _, err := metadata.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			repository, path, component, version string
		}{
			{"models", "models/core/0.2.0/core.glb", "models/core", "0.2.0"},
			{"models", "models/core/0.2.0/SHA256SUMS", "models/core", "0.2.0"},
			{"models", "docs/readme.txt", "", ""},
			{"plain", "models/core/0.2.0/core.glb", "", ""},
			{"images", "v2/team/app/manifests/1.2.3", "team/app", "1.2.3"},
			{"images", "v2/team/app/manifests/sha256:" + strings.Repeat("d", 64), "", ""},
		} {
			component, version := assetComponent(t, metadata, tc.repository, tc.path)
			if component != tc.component || version != tc.version {
				t.Errorf("%s/%s component = %q@%q, want %q@%q", tc.repository, tc.path, component, version, tc.component, tc.version)
			}
		}
	})
}

func TestAssetComponentColumnsFollowPatternChanges(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: "models", Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		putComponentTestAsset(t, metadata, "models", "models/core/0.2.0/core.glb", "raw", "")
		if component, _ := assetComponent(t, metadata, "models", "models/core/0.2.0/core.glb"); component != "" {
			t.Fatalf("component before patterns = %q", component)
		}
		repository, err := metadata.Repository(ctx, "models")
		if err != nil {
			t.Fatal(err)
		}
		repository.FormatConfig = componentTestConfig()
		if err := metadata.UpdateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
		if component, version := assetComponent(t, metadata, "models", "models/core/0.2.0/core.glb"); component != "models/core" || version != "0.2.0" {
			t.Fatalf("component after patterns = %q@%q", component, version)
		}
		repository.FormatConfig = nil
		if err := metadata.UpdateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
		if component, _ := assetComponent(t, metadata, "models", "models/core/0.2.0/core.glb"); component != "" {
			t.Fatalf("component after removing patterns = %q", component)
		}
	})
}

func TestComponentPageReadsOnlyPagedComponents(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: "models", Format: "raw", Type: "hosted", FormatConfig: componentTestConfig()}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{
			"models/a/1.0/a.glb", "models/a/1.1/a.glb",
			"models/b/1.0/b.glb", "models/b/1.0/SHA256SUMS",
			"models/c/2.0/c.glb", "docs/readme.txt",
		} {
			putComponentTestAsset(t, metadata, "models", path, "raw", "")
		}
		repository, err := metadata.Repository(ctx, "models")
		if err != nil {
			t.Fatal(err)
		}
		view := metadata.ForRepository(repository)
		var visited []string
		var afterID int64
		for pages := 0; ; pages++ {
			if pages > 4 {
				t.Fatal("component paging did not terminate")
			}
			page, err := view.ComponentPage(ctx, afterID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Components) != 1 {
				t.Fatalf("page components = %v", page.Components)
			}
			for _, asset := range page.Assets {
				if asset.Component != page.Components[0] {
					t.Fatalf("page for %q read unrelated asset %s", page.Components[0], asset.Path)
				}
				afterID = asset.ID
			}
			visited = append(visited, page.Components[0])
			if !page.HasMore {
				break
			}
		}
		if strings.Join(visited, ",") != "models/a,models/b,models/c" {
			t.Fatalf("visited components = %v", visited)
		}
	})
}

func TestProjectFromStoredComponentMatchesDerivedProjection(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "models", Format: "raw", Type: "hosted", FormatConfig: componentTestConfig()}); err != nil {
		t.Fatal(err)
	}
	repository, err := metadata.Repository(ctx, "models")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"models/core/0.2.0/core.glb", "models/core/0.2.0/SHA256SUMS", "docs/readme.txt"} {
		stored := putComponentTestAsset(t, metadata, "models", path, "raw", "")
		stored, err = metadata.Asset(ctx, "models", stored.Path)
		if err != nil {
			t.Fatal(err)
		}
		derived := stored
		derived.ComponentStored = false
		derived.Component, derived.ComponentVersion = "", ""
		got := assetattrs.Project(stored, repository)["raw"]
		want := assetattrs.Project(derived, repository)["raw"]
		if !mapsEqual(got, want) {
			t.Errorf("%s projection from row = %v, derived = %v", path, got, want)
		}
	}
}

func mapsEqual(left, right any) bool {
	leftMap, _ := left.(map[string]any)
	rightMap, _ := right.(map[string]any)
	if len(leftMap) != len(rightMap) {
		return false
	}
	for key, value := range leftMap {
		if rightMap[key] != value {
			return false
		}
	}
	return true
}
