package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestTimestampMigrationInventoryCoversSchema(t *testing.T) {
	store := openMigratedSQLite(t)
	ctx := context.Background()
	want := make(map[string]bool)
	for _, entry := range timestampColumns {
		for _, column := range entry.columns {
			want[entry.table+"."+column] = true
		}
	}
	tables, err := store.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		t.Fatal(err)
	}
	tables.Close()
	seen := make(map[string]bool)
	for _, table := range names {
		rows, err := store.db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, dataType string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			key := table + "." + name
			if want[key] {
				if dataType != "TEXT" {
					t.Errorf("%s type = %s, want TEXT", key, dataType)
				}
				seen[key] = true
			}
			if dataType == "TEXT" && (strings.HasSuffix(name, "_at") || strings.HasSuffix(name, "_until") || name == "last_accessed") && !want[key] {
				t.Errorf("timestamp %s is missing from migration inventory", key)
			}
		}
		if err := rows.Err(); err != nil {
			t.Error(err)
		}
		rows.Close()
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("listed timestamp %s is missing from schema", key)
		}
	}
	for _, dialect := range []string{dialectSQLite, dialectPostgres} {
		migration := schemaMigration{version: 13, name: "fixed_width_timestamps"}
		contents, err := migrationFiles.ReadFile(migrationPath(dialect, migration))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range timestampColumns {
			for _, column := range entry.columns {
				statement := "UPDATE " + entry.table + " SET " + column + " ="
				if !strings.Contains(string(contents), statement) {
					t.Errorf("%s migration omits %s.%s", dialect, entry.table, column)
				}
			}
		}
	}
}

func TestSQLTimestampOrderingOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *SQLStore, _ string) {
		ctx := context.Background()
		base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		for _, pair := range [][2]time.Time{
			{base, base.Add(time.Nanosecond)},
			{base.Add(90 * time.Millisecond), base.Add(900 * time.Millisecond)},
			{base.Add(900 * time.Millisecond), base.Add(900000001 * time.Nanosecond)},
			{base.Add(999999999 * time.Nanosecond), base.Add(time.Second)},
		} {
			if !(formatTime(pair[0]) < formatTime(pair[1])) {
				t.Fatalf("timestamps sort incorrectly: %s >= %s", formatTime(pair[0]), formatTime(pair[1]))
			}
		}
		for _, tc := range []struct {
			name, holder string
			expires, now time.Time
			want         bool
		}{
			{"live", "other", base.Add(900 * time.Millisecond), base, false},
			{"expired", "other", base, base.Add(500 * time.Millisecond), true},
		} {
			if won, err := store.AcquireLease(ctx, tc.name, "original", base.Add(-time.Second), tc.expires); err != nil || !won {
				t.Fatalf("initial lease %s: won=%v err=%v", tc.name, won, err)
			}
			if won, err := store.AcquireLease(ctx, tc.name, tc.holder, tc.now, tc.now.Add(time.Minute)); err != nil || won != tc.want {
				t.Fatalf("lease %s: won=%v err=%v, want %v", tc.name, won, err, tc.want)
			}
		}
		if err := store.CreateRepository(ctx, domain.Repository{Name: "timestamp-order", Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateWebhook(ctx, domain.Webhook{Name: "timestamp-order", URL: "https://example.org/hook", Secret: "timestamp-secret", Events: []string{domain.WebhookAssetUploaded}, Repositories: []string{"timestamp-order"}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := store.EnqueueWebhookEvent(ctx, domain.WebhookEvent{ID: "timestamp-order", Type: domain.WebhookAssetUploaded, Repository: "timestamp-order", OccurredAt: base.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
		first, err := store.ClaimWebhookDeliveries(ctx, "first", base.Add(-100*time.Millisecond), time.Second, 10)
		if err != nil || len(first) != 1 {
			t.Fatalf("first webhook claim: %d deliveries, err=%v", len(first), err)
		}
		second, err := store.ClaimWebhookDeliveries(ctx, "second", base, time.Second, 10)
		if err != nil || len(second) != 0 {
			t.Fatalf("live webhook lock reclaimed: %d deliveries, err=%v", len(second), err)
		}
		if err := createUploadTestRepository(ctx, store); err != nil {
			t.Fatal(err)
		}
		uploadRepo, err := store.Repository(ctx, "registry")
		if err != nil {
			t.Fatal(err)
		}
		session := testUploadSession("fractional-upload", "alice", base.Add(900*time.Millisecond))
		session.RepositoryID = uploadRepo.ID
		limits := UploadSessionLimits{MaxStagedBytes: 10, MaxPrincipalStagedBytes: 10, MaxPrincipalSessions: 4}
		if err := store.CreateUploadSession(ctx, session, limits); err != nil {
			t.Fatal(err)
		}
		for _, cutoff := range []struct {
			at   time.Time
			want int
		}{
			{base, 0},
			{base.Add(950 * time.Millisecond), 1},
		} {
			stale, err := store.StaleUploadSessions(ctx, "default", cutoff.at, base)
			if err != nil || len(stale) != cutoff.want {
				t.Fatalf("stale uploads before %s: count=%d, err=%v, want %d", cutoff.at, len(stale), err, cutoff.want)
			}
		}
	})
}

func TestTimestampMigrationPreservesValuesAndPredicatesOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *SQLStore, _ string) {
		ctx := context.Background()
		base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		// Recreate the pre-v13 ledger and persisted representation in this
		// isolated schema, then run the real migration path.
		if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 13`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `DROP INDEX idx_users_identity`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `ALTER TABLE users DROP COLUMN identity`); err != nil {
			t.Fatal(err)
		}
		for _, value := range []time.Time{base, base.Add(90 * time.Millisecond), base.Add(900 * time.Millisecond)} {
			name := fmt.Sprintf("lease-%d", value.Nanosecond())
			if _, err := store.db.ExecContext(ctx, `INSERT INTO leader_leases (name, holder, expires_at) VALUES (?, ?, ?)`, name, "old", value.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CreateRepository(ctx, domain.Repository{Name: "timestamp-migrate", Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		repository, err := store.Repository(ctx, "timestamp-migrate")
		if err != nil {
			t.Fatal(err)
		}
		asset, err := store.PutAsset(ctx, domain.Asset{Repository: repository.Name, RepositoryID: repository.ID, Path: "artifact", Digest: "old-digest"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE assets SET created_at = ?, updated_at = ?, validated_at = ?, last_accessed = ? WHERE id = ?`,
			base.Format(time.RFC3339Nano), base.Add(90*time.Millisecond).Format(time.RFC3339Nano),
			base.Add(900*time.Millisecond).Format(time.RFC3339Nano), base.Add(time.Nanosecond).Format(time.RFC3339Nano), asset.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO negative_cache (repository_id, path, expires_at) VALUES (?, ?, ?)`, repository.ID, "missing", base.Add(900*time.Millisecond).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		fraction := time.Second
		for width := 1; width <= 9; width++ {
			fraction /= 10
			value := base.Add(fraction)
			if _, err := store.db.ExecContext(ctx, `INSERT INTO negative_cache (repository_id, path, expires_at) VALUES (?, ?, ?)`, repository.ID, fmt.Sprintf("width-%d", width), value.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
		}
		// Asset writes above use the current columns; drop them only now so
		// migration 15 runs against its pre-migration shape.
		dropRawComponentSchema(t, store)
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for _, value := range []time.Time{base, base.Add(90 * time.Millisecond), base.Add(900 * time.Millisecond)} {
			name := fmt.Sprintf("lease-%d", value.Nanosecond())
			lease, err := store.Lease(ctx, name)
			if err != nil || !lease.ExpiresAt.Equal(value) {
				t.Fatalf("migrated lease %s: %+v, err=%v", name, lease, err)
			}
			var stored string
			if err := store.db.QueryRowContext(ctx, `SELECT expires_at FROM leader_leases WHERE name = ?`, name).Scan(&stored); err != nil || stored != formatTime(value) {
				t.Fatalf("stored lease %s = %q, err=%v", name, stored, err)
			}
		}
		if won, err := store.AcquireLease(ctx, "lease-900000000", "new", base, base.Add(time.Minute)); err != nil || won {
			t.Fatalf("migrated live lease stolen: won=%v, err=%v", won, err)
		}
		if won, err := store.AcquireLease(ctx, "lease-0", "new", base.Add(time.Nanosecond), base.Add(time.Minute)); err != nil || !won {
			t.Fatalf("migrated expired lease unavailable: won=%v, err=%v", won, err)
		}
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM negative_cache WHERE repository_id = ? AND expires_at > ?`, repository.ID, formatTime(base)).Scan(&count); err != nil || count != 10 {
			t.Fatalf("migrated negative-cache expiry count=%d, err=%v", count, err)
		}
		fraction = time.Second
		for width := 1; width <= 9; width++ {
			fraction /= 10
			var stored string
			if err := store.db.QueryRowContext(ctx, `SELECT expires_at FROM negative_cache WHERE repository_id = ? AND path = ?`, repository.ID, fmt.Sprintf("width-%d", width)).Scan(&stored); err != nil || stored != formatTime(base.Add(fraction)) {
				t.Fatalf("migrated %d-digit fraction = %q, err=%v", width, stored, err)
			}
		}
		migratedAsset, err := store.AssetByRepositoryID(ctx, repository.ID, "artifact")
		if err != nil || !migratedAsset.UpdatedAt.Equal(base.Add(90*time.Millisecond)) || migratedAsset.LastAccessed == nil || !migratedAsset.LastAccessed.Equal(base.Add(time.Nanosecond)) {
			t.Fatalf("migrated asset timestamps: %+v, err=%v", migratedAsset, err)
		}
		if deleted, err := store.DeleteAssetsIfUnchanged(ctx, []domain.Asset{migratedAsset}); err != nil || !deleted {
			t.Fatalf("migrated asset CAS delete: deleted=%v, err=%v", deleted, err)
		}
	})
}
