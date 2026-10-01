package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"time"
)

// SchemaError means the recorded migration history is incompatible with this
// binary. Reconnecting cannot repair it.
type SchemaError struct{ Err error }

func (e *SchemaError) Error() string { return e.Err.Error() }
func (e *SchemaError) Unwrap() error { return e.Err }

// migrationFiles contains the authoritative schema history for both supported
// database dialects. New schema changes must be appended as a new numbered file.
//
//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationFiles embed.FS

type schemaMigration struct {
	version int
	name    string
}

var schemaMigrations = []schemaMigration{
	{version: 1, name: "initial_schema"},
	{version: 2, name: "proxy_format_path"},
	{version: 3, name: "drop_provision_spec_hash"},
	{version: 4, name: "immutable_blob_store_definition"},
	{version: 5, name: "drop_default_managed_columns"},
	{version: 6, name: "drop_role_roles"},
	{version: 7, name: "asset_paging_index"},
	{version: 8, name: "repository_members"},
	{version: 9, name: "asset_dependency_blob_store"},
	{version: 10, name: "asset_public_path_index"},
	{version: 11, name: "repository_allow_overwrite"},
	{version: 12, name: "proxy_cache_publication"},
	{version: 13, name: "fixed_width_timestamps"},
	{version: 14, name: "local_account_identity"},
	{version: 15, name: "cleanup_policy_order"},
	{version: 16, name: "asset_components"},
}

const createSchemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`

type appliedMigration struct {
	name     string
	checksum string
}

// Migrate applies every schema migration in order inside one transaction.
func (s *SQLStore) Migrate(ctx context.Context) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s migration: %w", s.dialect, err)
	}
	defer func() {
		_ = transaction.Rollback()
	}()

	if s.dialect == dialectPostgres {
		const acquireLock = `SELECT pg_advisory_xact_lock(?)`
		if _, err := transaction.ExecContext(
			ctx,
			acquireLock,
			postgresMigrationLockID,
		); err != nil {
			return fmt.Errorf("acquire PostgreSQL migration lock: %w", err)
		}
	}

	if _, err := transaction.ExecContext(ctx, createSchemaMigrationsTable); err != nil {
		return fmt.Errorf("create schema migration ledger: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, transaction)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(s.dialect, applied); err != nil {
		return &SchemaError{Err: err}
	}

	for _, migration := range schemaMigrations {
		if _, exists := applied[migration.version]; exists {
			continue
		}
		if err := s.applyMigration(ctx, transaction, migration); err != nil {
			return err
		}
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit %s migrations: %w", s.dialect, err)
	}
	return nil
}

func loadAppliedMigrations(
	ctx context.Context,
	transaction *dialectTx,
) (map[int]appliedMigration, error) {
	rows, err := transaction.QueryContext(
		ctx,
		`SELECT version, name, checksum FROM schema_migrations ORDER BY version`,
	)
	if err != nil {
		return nil, fmt.Errorf("list applied schema migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]appliedMigration)
	for rows.Next() {
		var version int
		var migration appliedMigration
		if err := rows.Scan(&version, &migration.name, &migration.checksum); err != nil {
			return nil, &SchemaError{Err: fmt.Errorf("scan applied schema migration: %w", err)}
		}
		applied[version] = migration
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied schema migrations: %w", err)
	}
	return applied, nil
}

func validateAppliedMigrations(
	dialect string,
	applied map[int]appliedMigration,
) error {
	known := make(map[int]string, len(schemaMigrations))
	for _, migration := range schemaMigrations {
		known[migration.version] = migration.name
	}
	for version, recorded := range applied {
		expectedName, exists := known[version]
		if !exists {
			return fmt.Errorf(
				"database schema migration %d (%s) is newer than this binary",
				version,
				recorded.name,
			)
		}
		if recorded.name != expectedName {
			return fmt.Errorf(
				"database schema migration %d is named %q, expected %q",
				version,
				recorded.name,
				expectedName,
			)
		}
		migration := schemaMigration{version: version, name: expectedName}
		contents, err := migrationFiles.ReadFile(migrationPath(dialect, migration))
		if err != nil {
			return fmt.Errorf("read applied schema migration %d: %w", version, err)
		}
		if recorded.checksum != migrationChecksum(contents) {
			return fmt.Errorf("database schema migration %d checksum does not match", version)
		}
	}

	missingEarlierMigration := false
	for _, migration := range schemaMigrations {
		_, exists := applied[migration.version]
		if !exists {
			missingEarlierMigration = true
			continue
		}
		if missingEarlierMigration {
			return fmt.Errorf(
				"database schema migration %d was applied out of order",
				migration.version,
			)
		}
	}
	return nil
}

func (s *SQLStore) applyMigration(
	ctx context.Context,
	transaction *dialectTx,
	migration schemaMigration,
) error {
	path := migrationPath(s.dialect, migration)
	statements, err := migrationFiles.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read schema migration %d: %w", migration.version, err)
	}
	if migration.version == 13 {
		if err := validateLegacyTimestamps(ctx, transaction); err != nil {
			return fmt.Errorf("validate stored timestamps: %w", err)
		}
	}
	if _, err := transaction.ExecContext(ctx, string(statements)); err != nil {
		return fmt.Errorf(
			"apply schema migration %d (%s): %w",
			migration.version,
			migration.name,
			err,
		)
	}

	if migration.version == 16 {
		if err := backfillAssetComponents(ctx, transaction); err != nil {
			return fmt.Errorf("backfill asset components: %w", err)
		}
	}

	if _, err := transaction.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (
            version, name, checksum, applied_at
        ) VALUES (?, ?, ?, ?)`,
		migration.version,
		migration.name,
		migrationChecksum(statements),
		formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("record schema migration %d: %w", migration.version, err)
	}
	return nil
}

func migrationChecksum(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func migrationPath(dialect string, migration schemaMigration) string {
	return fmt.Sprintf(
		"migrations/%s/%04d_%s.sql",
		dialect,
		migration.version,
		migration.name,
	)
}
