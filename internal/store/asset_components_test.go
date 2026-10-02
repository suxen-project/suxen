package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	"github.com/suxen-project/suxen/internal/retention"
	"github.com/suxen-project/suxen/internal/versionorder"
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

func assetVersionKey(t *testing.T, metadata *SQLStore, repository, path string) string {
	t.Helper()
	var key string
	if err := metadata.db.QueryRowContext(context.Background(),
		`SELECT component_version_key FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`,
		repository, path,
	).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestRawComponentMigrationBackfillsExistingRows(t *testing.T) {
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
		putComponentTestAsset(t, metadata, "plain", "readme.txt", "raw", "")
		putComponentTestAsset(t, metadata, "images", "v2/team/app/manifests/1.2.3", "oci-manifest", "1.2.3")
		putComponentTestAsset(t, metadata, "images", "v2/team/app/manifests/sha256:"+strings.Repeat("d", 64), "oci-manifest", "sha256:"+strings.Repeat("d", 64))

		// Recreate the pre-15 shape with the rows above, then migrate forward.
		if _, err := metadata.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 15`); err != nil {
			t.Fatal(err)
		}
		dropRawComponentSchema(t, metadata)
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			repository, path, component, version, group string
		}{
			{"models", "models/core/0.2.0/core.glb", "models/core", "0.2.0", retention.StoredKey(retention.RawGroupKey(rawcomponent.Match{Name: "models/core"}))},
			{"models", "models/core/0.2.0/SHA256SUMS", "models/core", "0.2.0", retention.StoredKey(retention.RawGroupKey(rawcomponent.Match{Name: "models/core"}))},
			{"models", "docs/readme.txt", "docs", "readme.txt", retention.StoredKey(retention.RawGroupKey(rawcomponent.Match{Name: "docs", Implicit: true}))},
			{"plain", "models/core/0.2.0/core.glb", "models/core/0.2.0", "core.glb", retention.StoredKey(retention.RawGroupKey(rawcomponent.Match{Name: "models/core/0.2.0", Implicit: true}))},
			{"plain", "readme.txt", "/", "readme.txt", retention.StoredKey(retention.RawGroupKey(rawcomponent.Match{Name: "/", Implicit: true}))},
			{"images", "v2/team/app/manifests/1.2.3", "", "", "gteam/app"},
			{"images", "v2/team/app/manifests/sha256:" + strings.Repeat("d", 64), "", "", ""},
		} {
			component, version := assetComponent(t, metadata, tc.repository, tc.path)
			if component != tc.component || version != tc.version {
				t.Errorf("%s/%s component = %q@%q, want %q@%q", tc.repository, tc.path, component, version, tc.component, tc.version)
			}
			wantKey := ""
			if tc.component != "" {
				wantKey = versionorder.Key(tc.version)
			}
			if key := assetVersionKey(t, metadata, tc.repository, tc.path); key != wantKey {
				t.Errorf("%s/%s version key = %q, want %q", tc.repository, tc.path, key, wantKey)
			}
			var group string
			if err := metadata.db.QueryRowContext(ctx,
				`SELECT retention_group FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`,
				tc.repository, tc.path,
			).Scan(&group); err != nil {
				t.Fatal(err)
			}
			if group != tc.group {
				t.Errorf("%s/%s retention group = %q, want %q", tc.repository, tc.path, group, tc.group)
			}
		}
	})
}

func TestDerivedColumnsRecomputeWhenRevisionChanges(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: "files", Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		putComponentTestAsset(t, metadata, "files", "dist/app.zip", "raw", "")
		// Simulate rows computed by an older grouping: a stale group and an
		// older revision.
		if _, err := metadata.db.ExecContext(ctx, `UPDATE assets SET retention_group = 'gstale', component = ''`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `UPDATE repositories SET derived_revision = 'old'`); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if component, version := assetComponent(t, metadata, "files", "dist/app.zip"); component != "dist" || version != "app.zip" {
			t.Fatalf("component after reconcile = %q@%q", component, version)
		}
		var revision string
		if err := metadata.db.QueryRowContext(ctx, `SELECT derived_revision FROM repositories WHERE name = 'files'`).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if revision != derivedRevision("raw") {
			t.Fatalf("derived revision = %q", revision)
		}
		// A repository already at the running revision is left alone.
		if _, err := metadata.db.ExecContext(ctx, `UPDATE assets SET retention_group = 'gkept'`); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		var group string
		if err := metadata.db.QueryRowContext(ctx, `SELECT retention_group FROM assets`).Scan(&group); err != nil || group != "gkept" {
			t.Fatalf("current repository was recomputed: group = %q, err = %v", group, err)
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
		if component, version := assetComponent(t, metadata, "models", "models/core/0.2.0/core.glb"); component != "models/core/0.2.0" || version != "core.glb" {
			t.Fatalf("component before patterns = %q@%q", component, version)
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
		if component, _ := assetComponent(t, metadata, "models", "models/core/0.2.0/core.glb"); component != "models/core/0.2.0" {
			t.Fatalf("component after removing patterns = %q", component)
		}
	})
}

func TestComponentVersionPageOrdersHighestVersionFirst(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: "models", Format: "raw", Type: "hosted", FormatConfig: componentTestConfig()}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{
			"models/a/1.9/a.glb", "models/a/1.10/a.glb", "models/a/1.10/SHA256SUMS",
			"models/b/1.0/b.glb", "docs/readme.txt",
		} {
			putComponentTestAsset(t, metadata, "models", path, "raw", "")
		}
		repository, err := metadata.Repository(ctx, "models")
		if err != nil {
			t.Fatal(err)
		}
		view := metadata.ForRepository(repository)
		var visited []string
		var after *ComponentVersion
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("component version paging did not terminate")
			}
			page, err := view.ComponentVersionPage(ctx, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Versions) != 1 {
				t.Fatalf("page versions = %v", page.Versions)
			}
			version := page.Versions[0]
			for _, asset := range page.Assets {
				if asset.Component != version.Component || asset.ComponentVersion != version.Version {
					t.Fatalf("page for %v read unrelated asset %s", version, asset.Path)
				}
			}
			visited = append(visited, fmt.Sprintf("%s@%s:%d", version.Component, version.Version, len(page.Assets)))
			if !page.HasMore {
				break
			}
			after = &version
		}
		if got := strings.Join(visited, ","); got != "docs@readme.txt:1,models/a@1.10:2,models/a@1.9:1,models/b@1.0:1" {
			t.Fatalf("visited versions = %s", got)
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

// dropRawComponentSchema undoes migration 15's schema so a test can rerun it
// against rows written by the current code. PostgreSQL keeps the path
// collation, which the migration sets idempotently.
func dropRawComponentSchema(t *testing.T, metadata *SQLStore) {
	t.Helper()
	for _, statement := range []string{
		`DROP INDEX idx_assets_component_versions`,
		`DROP INDEX idx_assets_retention_group`,
		`ALTER TABLE cleanup_policies DROP COLUMN retention_order`,
		`ALTER TABLE assets DROP COLUMN component`,
		`ALTER TABLE assets DROP COLUMN component_version`,
		`ALTER TABLE assets DROP COLUMN component_version_key`,
		`ALTER TABLE assets DROP COLUMN retention_group`,
		`ALTER TABLE repositories DROP COLUMN derived_revision`,
	} {
		if _, err := metadata.db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}
