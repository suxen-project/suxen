package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSQLiteMigrationsRecordBaselineOnce(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()

	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := metadata.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM schema_migrations`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(schemaMigrations) {
		t.Fatalf("recorded %d schema migrations, want %d", count, len(schemaMigrations))
	}

	var name string
	if err := metadata.db.QueryRowContext(
		ctx,
		`SELECT name FROM schema_migrations WHERE version = 1`,
	).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "initial_schema" {
		t.Fatalf("migration 1 is named %q", name)
	}
}

func TestSQLiteMigrationsRejectUnknownVersion(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (
            version, name, checksum, applied_at
        ) VALUES (?, ?, ?, ?)`,
		99,
		"future_schema",
		"not-used-for-an-unknown-version",
		"2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	err := metadata.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("Migrate() error = %v, want newer-schema rejection", err)
	}
	var schema *SchemaError
	if !errors.As(err, &schema) {
		t.Fatalf("Migrate() error = %T, want SchemaError", err)
	}
}

func TestSQLiteMigrationsRejectChangedHistory(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(
		ctx,
		`UPDATE schema_migrations SET checksum = ? WHERE version = 1`,
		"changed",
	); err != nil {
		t.Fatal(err)
	}

	err := metadata.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "checksum does not match") {
		t.Fatalf("Migrate() error = %v, want checksum rejection", err)
	}
	var schema *SchemaError
	if !errors.As(err, &schema) {
		t.Fatalf("Migrate() error = %T, want SchemaError", err)
	}
}

func TestSQLiteInitialSchemaHasFinalShape(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()

	columnExists := func(table, column string) bool {
		t.Helper()

		var count int
		if err := metadata.db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			table,
			column,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}
	tableExists := func(table string) bool {
		t.Helper()

		var count int
		if err := metadata.db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}

	for _, expected := range []struct {
		table  string
		column string
	}{
		{table: "repositories", column: "endpoints"},
		{table: "assets", column: "blob_store"},
		{table: "assets", column: "format_path"},
		{table: "assets", column: "validated_at"},
		{table: "assets", column: "last_accessed"},
		{table: "oidc_providers", column: "allow_password_grant"},
		{table: "classification_rules", column: "inherit_global"},
		{table: "download_gates", column: "inherit_global"},
		{table: "blob_stores", column: "state"},
		{table: "blob_stores", column: "drain_target"},
	} {
		if !columnExists(expected.table, expected.column) {
			t.Errorf("%s is missing the %s column", expected.table, expected.column)
		}
	}

	for _, removed := range []struct {
		table  string
		column string
	}{
		{table: "assets", column: "classification"},
		{table: "assets", column: "last_downloaded"},
		{table: "classification_rules", column: "default_label"},
		{table: "download_gates", column: "namespace"},
	} {
		if columnExists(removed.table, removed.column) {
			t.Errorf("%s still has the removed %s column", removed.table, removed.column)
		}
	}

	for _, table := range []string{
		"classification_defaults",
		"download_gate_defaults",
		"trust_policy_defaults",
		"upload_sessions",
	} {
		if !tableExists(table) {
			t.Errorf("%s table is missing", table)
		}
	}

	var stores int
	if err := metadata.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM blob_stores`,
	).Scan(&stores); err != nil {
		t.Fatal(err)
	}
	if stores != 1 {
		t.Fatalf("blob_stores = %d, want the test default only", stores)
	}

	var obsoleteInsertTrigger int
	if err := metadata.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'assets_blob_store_default'`,
	).Scan(&obsoleteInsertTrigger); err != nil {
		t.Fatal(err)
	}
	if obsoleteInsertTrigger != 0 {
		t.Fatal("obsolete asset insert trigger still exists")
	}
}

func TestEmbeddedMigrationExistsForEveryDialect(t *testing.T) {
	for _, dialect := range []string{dialectSQLite, dialectPostgres} {
		for _, migration := range schemaMigrations {
			path := migrationPath(dialect, migration)
			contents, err := migrationFiles.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if len(contents) == 0 {
				t.Fatalf("embedded migration %s is empty", path)
			}
		}
	}
}

func TestSchemaMigrationHistoryIsContiguous(t *testing.T) {
	for index, migration := range schemaMigrations {
		wantVersion := index + 1
		if migration.version != wantVersion {
			t.Fatalf(
				"schema migration at index %d has version %d, want %d",
				index,
				migration.version,
				wantVersion,
			)
		}
	}
}
