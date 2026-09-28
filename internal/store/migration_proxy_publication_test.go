package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Replays the 0011 -> 0012 upgrade against existing repository, asset, and
// negative-cache rows. Migration 0012 is additive; those rows must survive.
func TestProxyPublicationMigrationPreservesPriorCacheOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := fmt.Sprintf("proxy-upgrade-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
			t.Fatal(err)
		}
		repo, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.PutAsset(ctx, domain.Asset{Repository: name, RepositoryID: repo.ID, Path: "present", Digest: "prior-digest"}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.PutNegativeCacheByRepositoryID(ctx, repo.ID, "absent", time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DROP TABLE proxy_cache_state`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DROP INDEX negative_cache_expiry`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 12`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DROP INDEX idx_users_identity`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `ALTER TABLE users DROP COLUMN identity`); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		asset, err := metadata.AssetByRepositoryID(ctx, repo.ID, "present")
		if err != nil || asset.Digest != "prior-digest" {
			t.Fatalf("prior asset=%+v err=%v", asset, err)
		}
		if hit, err := metadata.NegativeCacheHitByRepositoryID(ctx, repo.ID, "absent", time.Now()); err != nil || !hit {
			t.Fatalf("prior negative cache hit=%v err=%v", hit, err)
		}
		var count int
		if err := metadata.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_cache_state`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("new state table count=%d err=%v", count, err)
		}
		if _, err := forRepository(metadata, repo).BeginProxyFetch(ctx, "present", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("upgraded state unusable: %v", err)
		}
	})
}
