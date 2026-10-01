package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
	spiformat "github.com/suxen-project/suxen/spi/format"
	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

const (
	passwordAlgorithm  = "argon2id"
	passwordIterations = 3
	passwordMemory     = 64 * 1024
	passwordThreads    = 4
	passwordSaltBytes  = 16
	passwordKeyBytes   = 32
)

const repositoryColumns = `
    id,
    name,
    format,
    type,
    blob_store,
    upstream,
    members,
    writable,
    allow_overwrite,
    format_config,
    endpoints,
    created_at`

// assetColumns projects the public repository name through the repository_id
// foreign key, so callers keep receiving name-addressed assets while ownership
// is keyed on the internal id. The correlated subquery keeps every asset query
// single-table, avoiding a join alias on the many WHERE clauses.
const assetColumns = `
    id,
    (SELECT name FROM repositories WHERE id = assets.repository_id),
    path,
	format_path,
    digest,
    size,
    blob_store,
    content_type,
    kind,
    reference,
    subject_digest,
    attributes,
    created_at,
    updated_at,
    validated_at,
    last_accessed`

// assetColumnsForReturning lists the raw asset columns for a DELETE ... RETURNING,
// where SQLite forbids the correlated subquery above. The repository name is set
// from the caller's known repository after scanning.
const assetColumnsForReturning = `
    id,
    repository_id,
    path,
	format_path,
    digest,
    size,
    blob_store,
    content_type,
    kind,
    reference,
    subject_digest,
    attributes,
    created_at,
    updated_at,
    validated_at,
    last_accessed`

// SQLStore implements Store for SQLite files and, via embedding, PostgreSQL.
type SQLStore struct {
	db      *dialectDB
	dialect string
}

// OpenSQLite opens a SQLite metadata store at path.
func OpenSQLite(path string) (*SQLStore, error) {
	dataSourceName := path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"

	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	database.SetMaxOpenConns(8)
	database.SetMaxIdleConns(4)
	database.SetConnMaxLifetime(time.Hour)

	return &SQLStore{
		db:      newDialectDB(database, dialectSQLite),
		dialect: dialectSQLite,
	}, nil
}

var _ Store = (*SQLStore)(nil)

// Close releases the metadata database connection pool.
func (s *SQLStore) Close() error {
	return s.db.Close()
}

// DatabasePoolStats returns the current database/sql pool state without querying the
// metadata database.
func (s *SQLStore) DatabasePoolStats() DatabasePoolStats {
	return DatabasePoolStats{
		Backend: s.dialect,
		Stats:   s.db.database.Stats(),
	}
}

// Ready checks that the metadata database can answer queries.
func (s *SQLStore) Ready(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// newRepositoryID returns an opaque internal identity for a repository. It is
// durable for the repository's lifetime and never reused: a delete+recreate
// under the same public name receives a fresh id so no owned row or in-flight
// operation from the old identity can attach to the new one.
func newRepositoryID() string {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("repo-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

// newUserIdentity creates a durable, unguessable generation for one account.
func newUserIdentity() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate account identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// CreateRepository inserts a repository definition.
func (s *SQLStore) CreateRepository(ctx context.Context, repository domain.Repository) error {
	if err := repository.Validate(); err != nil {
		return err
	}
	normalizeRepository(&repository)
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
		return err
	}
	if err := s.insertRepositoryTx(ctx, transaction, repository); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	if repository.Upstream != "" {
		return s.invalidateProvisionSecret(ctx, "repository", repository.Name)
	}
	return nil
}

// RepositorySave is an atomic repository mutation and its ownership effect.
// Create inserts a new repository; an update replaces the definition in place.
type RepositorySave struct {
	Repository domain.Repository
	Create     bool
	Ownership  Ownership
	// PreserveUpstream retains the complete upstream URL on updates, including
	// secrets that are intentionally absent from HTTP resource representations.
	PreserveUpstream bool
}

// SaveRepository commits a repository row and its ownership record in one
// transaction serialized on ("repository", name). The keyed ownership lock is
// taken first, then the global repository-relations lock, in that order for
// every repository mutation so the two lock regimes cannot deadlock. An
// imperative mutation of a provisioning-managed repository returns
// domain.ErrManaged unless Force is set.
func (s *SQLStore) SaveRepository(ctx context.Context, save RepositorySave) error {
	repository := save.Repository
	preserveUpstream := save.PreserveUpstream && !save.Create
	if !preserveUpstream {
		if err := repository.Validate(); err != nil {
			return err
		}
	}
	if save.Create {
		normalizeRepository(&repository)
	} else {
		normalizeRepositoryBase(&repository)
	}
	return s.writeOwned(ctx, "repository", repository.Name, save.Ownership, !save.Create, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
				return err
			}
			if preserveUpstream {
				if err := transaction.QueryRowContext(ctx,
					`SELECT upstream FROM repositories WHERE name = ?`, repository.Name,
				).Scan(&repository.Upstream); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return domain.ErrNotFound
					}
					return err
				}
				if err := repository.Validate(); err != nil {
					return err
				}
			}
			if save.Create {
				return s.insertRepositoryTx(ctx, transaction, repository)
			}
			return s.updateRepositoryTx(ctx, transaction, repository)
		})
}

// insertRepositoryTx inserts a repository row within a caller's transaction that
// already holds the repository-relations lock. The blob store must exist and be
// active; group members and OCI endpoints are validated under the same lock.
func (s *SQLStore) insertRepositoryTx(ctx context.Context, transaction *dialectTx, repository domain.Repository) error {
	var state string
	stateQuery := `SELECT state FROM blob_stores WHERE name = ?`
	if s.dialect == dialectPostgres {
		// Drain start updates this row. A shared lock keeps it active through
		// insertion and remains compatible with the FK's key-share lock.
		stateQuery += ` FOR SHARE`
	}
	if err := transaction.QueryRowContext(
		ctx,
		stateQuery,
		repository.BlobStore,
	).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if state != domain.BlobStoreStateActive {
		return domain.ErrBlobStoreDraining
	}
	if err := s.validateRepositoryMembers(ctx, transaction, repository); err != nil {
		return err
	}
	if err := s.rejectConflictingOCIEndpoints(ctx, transaction, repository); err != nil {
		return err
	}
	members, err := json.Marshal(repository.Members)
	if err != nil {
		return fmt.Errorf("encode repository members: %w", err)
	}
	formatConfig, err := encodeFormatConfig(repository.FormatConfig)
	if err != nil {
		return err
	}
	endpoints, err := encodeRepositoryEndpoints(repository.Endpoints)
	if err != nil {
		return err
	}
	const query = `
        INSERT INTO repositories (
            id, name, format, type, blob_store, upstream, members, writable,
            allow_overwrite, format_config, endpoints, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = transaction.ExecContext(
		ctx,
		query,
		newRepositoryID(),
		repository.Name,
		repository.Format,
		repository.Type,
		repository.BlobStore,
		repository.Upstream,
		string(members),
		repository.Writable,
		repositoryAllowOverwrite(repository),
		formatConfig,
		endpoints,
		formatTime(repository.CreatedAt),
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	if isBlobStoreReferenceError(err) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	return replaceRepositoryMembersTx(ctx, transaction, repository)
}

// replaceRepositoryMembersTx rewrites a repository's membership rows to match its
// current member list. Group membership is dual-written here alongside the
// repositories.members JSON column: the column stays the authoritative source for
// a repository's own member list, and the relation is a reverse index populated
// for a later release to consume.
//
// Only a group's rows are written, matching the migration backfill's
// WHERE type = 'group' filter, so the relation has one representation regardless
// of whether a row came from backfill or a live write — repository validation
// permits a non-group to carry a member list, but such a list is not group
// membership and must not appear in the relation. The DELETE runs unconditionally
// so a stale non-group list (or an emptied group) is cleared.
//
// A member name is written at its first-occurrence index in the list, matching the
// backfill's MIN(ordinality) so the two paths agree: a historical list may repeat
// a name (member validation does not reject duplicates) while the relation's
// (group_name, member_name) primary key is a set, so a repeat is skipped but does
// not shift a later member's position.
func replaceRepositoryMembersTx(ctx context.Context, executor sqlExecer, repository domain.Repository) error {
	if _, err := executor.ExecContext(ctx,
		`DELETE FROM repository_members WHERE group_name = ?`, repository.Name,
	); err != nil {
		return err
	}
	if repository.Type != "group" {
		return nil
	}
	seen := make(map[string]struct{}, len(repository.Members))
	for index, member := range repository.Members {
		if _, duplicate := seen[member]; duplicate {
			continue
		}
		seen[member] = struct{}{}
		if _, err := executor.ExecContext(ctx,
			`INSERT INTO repository_members (group_name, member_name, position) VALUES (?, ?, ?)`,
			repository.Name, member, index,
		); err != nil {
			return err
		}
	}
	return nil
}

// UpdateRepository replaces a repository definition while preserving its name.
func (s *SQLStore) UpdateRepository(ctx context.Context, repository domain.Repository) error {
	if err := repository.Validate(); err != nil {
		return err
	}
	normalizeRepositoryBase(&repository)
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
		return err
	}
	if err := s.updateRepositoryTx(ctx, transaction, repository); err != nil {
		return err
	}
	return transaction.Commit()
}

// updateRepositoryTx replaces a repository definition within a caller's
// transaction that already holds the repository-relations lock. Format, type,
// and the upstream endpoint are immutable; a blob-store rebind is allowed only
// while the repository holds no assets. A credential rotation clears the
// provisioning secret fingerprint so provisioning re-detects the rotated secret.
func (s *SQLStore) updateRepositoryTx(ctx context.Context, transaction *dialectTx, repository domain.Repository) error {
	members, err := json.Marshal(repository.Members)
	if err != nil {
		return fmt.Errorf("encode repository members: %w", err)
	}
	if err := s.validateRepositoryMembers(ctx, transaction, repository); err != nil {
		return err
	}
	// Raw component rules feed classification, so a change relabels assets.
	// Take the relabel lock before any row lock: publications hold it shared
	// while their asset inserts key-share lock this repository row.
	if repository.Format == "raw" {
		if err := lockClassificationRelabel(ctx, transaction); err != nil {
			return err
		}
	}

	var targetBlobStore string
	var targetBlobStoreState string
	targetQuery := `SELECT name, state FROM blob_stores WHERE name = ?`
	if s.dialect == dialectPostgres {
		// The shared lock serializes a rebind with drain start without blocking
		// another repository write or the migration's FK key-share lookup.
		targetQuery += ` FOR SHARE`
	}
	if err := transaction.QueryRowContext(
		ctx,
		targetQuery,
		repository.BlobStore,
	).Scan(&targetBlobStore, &targetBlobStoreState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	currentQuery := `SELECT id, blob_store, upstream, format, type, allow_overwrite, format_config FROM repositories WHERE name = ?`
	if s.dialect == dialectPostgres {
		currentQuery += ` FOR UPDATE`
	}
	var currentID string
	var currentBlobStore string
	var currentUpstream string
	var currentFormat string
	var currentType string
	var currentAllowOverwrite bool
	var currentFormatConfig string
	if err := transaction.QueryRowContext(
		ctx,
		currentQuery,
		repository.Name,
	).Scan(&currentID, &currentBlobStore, &currentUpstream, &currentFormat, &currentType, &currentAllowOverwrite, &currentFormatConfig); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if repository.Type == "hosted" && repository.AllowOverwrite == nil {
		repository.AllowOverwrite = &currentAllowOverwrite
	}
	// Type and format are fixed at creation: they determine the resolution and
	// storage model, and changing them would strand assets under the old model.
	if repository.Format != currentFormat || repository.Type != currentType {
		return domain.ErrImmutableRepositoryField
	}
	// The upstream endpoint is fixed at creation: repointing a proxy at a
	// different origin would keep serving the old origin's cached content.
	// Only the embedded credentials (URL userinfo) may rotate.
	sameEndpoint, err := domain.SameUpstreamEndpoint(currentUpstream, repository.Upstream)
	if err != nil {
		return err
	}
	if !sameEndpoint {
		return domain.ErrImmutableRepositoryField
	}
	if repository.Type == "group" {
		var policies int
		if err := transaction.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM trust_policies WHERE repository_id = ?`, currentID,
		).Scan(&policies); err != nil {
			return err
		}
		if policies > 0 {
			return domain.ErrInvalidTrustPolicy
		}
	}
	if currentBlobStore != repository.BlobStore {
		if targetBlobStoreState != domain.BlobStoreStateActive {
			return domain.ErrBlobStoreDraining
		}
		var assets int
		if err := transaction.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM assets WHERE repository_id = ?`,
			currentID,
		).Scan(&assets); err != nil {
			return err
		}
		if assets > 0 {
			return domain.ErrRepositoryBlobStoreInUse
		}
	}
	formatConfig, err := encodeFormatConfig(repository.FormatConfig)
	if err != nil {
		return err
	}
	endpoints, err := encodeRepositoryEndpoints(repository.Endpoints)
	if err != nil {
		return err
	}
	if err := s.rejectConflictingOCIEndpoints(ctx, transaction, repository); err != nil {
		return err
	}
	const query = `
        UPDATE repositories
        SET format = ?, type = ?, blob_store = ?, upstream = ?, members = ?,
            writable = ?, allow_overwrite = ?, format_config = ?, endpoints = ?
        WHERE name = ?`
	result, err := transaction.ExecContext(
		ctx,
		query,
		repository.Format,
		repository.Type,
		repository.BlobStore,
		repository.Upstream,
		string(members),
		repository.Writable,
		repositoryAllowOverwrite(repository),
		formatConfig,
		endpoints,
		repository.Name,
	)
	if err != nil {
		if isRepositoryBlobStoreInUseError(err) {
			return domain.ErrRepositoryBlobStoreInUse
		}
		if isBlobStoreReferenceError(err) {
			return domain.ErrNotFound
		}
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return err
	}
	if err := replaceRepositoryMembersTx(ctx, transaction, repository); err != nil {
		return err
	}
	if repository.Format == "raw" && formatConfig != currentFormatConfig {
		if err := relabelRepositoryTx(ctx, transaction, repository.Name); err != nil {
			return err
		}
	}
	// The endpoint is immutable (checked above), so any remaining upstream
	// difference is a credential rotation in the URL userinfo. Clear the
	// provisioning secret fingerprint so the rotated secret is re-detected.
	if currentUpstream != repository.Upstream {
		if _, err := transaction.ExecContext(
			ctx,
			`UPDATE provision_records
			 SET secret_fingerprint = '', updated_at = ?
			 WHERE kind = 'repository' AND name = ?`,
			formatTime(time.Now().UTC()),
			repository.Name,
		); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRepository deletes a repository and its asset metadata, folding the
// removal of its ownership record into the same transaction serialized on
// ("repository", name). It refuses to delete a repository still referenced as a
// member by any group or named by a cleanup policy or webhook. Reference
// checks and deletion share the repository-relations lock with reference writes.
func (s *SQLStore) DeleteRepository(ctx context.Context, name string, ownership Ownership) error {
	return s.writeOwned(ctx, "repository", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
				return err
			}
			if err := s.rejectGroupReferences(ctx, transaction, name); err != nil {
				return err
			}
			if err := rejectRepositoryPolicyReferences(ctx, transaction, name); err != nil {
				return err
			}
			var uploadSessions int
			if err := transaction.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM upload_sessions WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)`, name,
			).Scan(&uploadSessions); err != nil {
				return err
			}
			if uploadSessions != 0 {
				return domain.ErrRepositoryHasUploadSessions
			}
			result, err := transaction.ExecContext(ctx, `DELETE FROM repositories WHERE name = ?`, name)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

// The policy and webhook filters are JSON arrays on both SQL backends. Decode
// them here so deletion uses the same exact-name matching as their consumers.
func rejectRepositoryPolicyReferences(ctx context.Context, transaction *dialectTx, name string) error {
	for _, reference := range []struct {
		table   string
		failure error
	}{
		{"cleanup_policies", domain.ErrRepositoryInUseByCleanupPolicy},
		{"webhooks", domain.ErrRepositoryInUseByWebhook},
	} {
		rows, err := transaction.QueryContext(ctx, "SELECT repositories FROM "+reference.table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var encoded string
			if err := rows.Scan(&encoded); err != nil {
				rows.Close()
				return err
			}
			var repositories []string
			if err := json.Unmarshal([]byte(encoded), &repositories); err != nil {
				rows.Close()
				return fmt.Errorf("decode %s repositories: %w", reference.table, err)
			}
			for _, repository := range repositories {
				if repository == name {
					rows.Close()
					return reference.failure
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func validateRepositoryFilterTx(ctx context.Context, transaction *dialectTx, names []string) error {
	for _, name := range uniqueStrings(names) {
		var found string
		err := transaction.QueryRowContext(ctx, `SELECT name FROM repositories WHERE name = ?`, name).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resolve repository %q: %w", name, domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// PostgreSQL's transaction advisory lock serializes all repository relation
// changes, including creation of a group which has no row to lock yet. SQLite
// uses an immediate write transaction for the same purpose.
func (s *SQLStore) lockRepositoryRelations(ctx context.Context, transaction *dialectTx) error {
	if s.dialect != dialectPostgres {
		return nil
	}
	_, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(782637294241)`)
	return err
}

func (s *SQLStore) validateRepositoryMembers(ctx context.Context, transaction *dialectTx, repository domain.Repository) error {
	if repository.Type != "group" {
		return nil
	}
	for _, name := range repository.Members {
		if name == repository.Name {
			return domain.ErrNestedGroupMember
		}
		var format, kind string
		err := transaction.QueryRowContext(ctx,
			`SELECT format, type FROM repositories WHERE name = ?`, name,
		).Scan(&format, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: group member %q does not exist", domain.ErrInvalidRepository, name)
		}
		if err != nil {
			return err
		}
		if kind == "group" {
			return fmt.Errorf("group member %q: %w", name, domain.ErrNestedGroupMember)
		}
		if format != repository.Format || kind != "hosted" && kind != "proxy" {
			return fmt.Errorf("group member %q: %w", name, domain.ErrInvalidRepository)
		}
	}
	return nil
}

// rejectGroupReferences returns ErrRepositoryInUseByGroup if any group lists
// name as a member. It scans group member JSON rather than the repository_members
// relation: during a rolling deployment an old binary writes only the JSON column
// and does not maintain the relation, so the relation can diverge from the
// authoritative list and cannot yet be trusted for a deletion-safety decision.
// The JSON column is authoritative and every binary maintains it, so the scan is
// correct across mixed versions. Group rows are locked on PostgreSQL so a
// concurrent update adding the member serializes against the delete.
func (s *SQLStore) rejectGroupReferences(
	ctx context.Context,
	transaction queryer,
	name string,
) error {
	query := `SELECT members FROM repositories WHERE type = 'group'`
	if s.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	rows, err := transaction.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return err
		}
		var members []string
		if err := json.Unmarshal([]byte(encoded), &members); err != nil {
			return fmt.Errorf("decode group members: %w", err)
		}
		for _, member := range members {
			if member == name {
				return domain.ErrRepositoryInUseByGroup
			}
		}
	}
	return rows.Err()
}

// Repository returns one repository by name.
func (s *SQLStore) Repository(ctx context.Context, name string) (domain.Repository, error) {
	return repositoryFrom(ctx, s.db, name)
}

func repositoryFrom(ctx context.Context, reader classificationReader, name string) (domain.Repository, error) {
	query := `SELECT ` + repositoryColumns + ` FROM repositories WHERE name = ?`
	return scanRepository(reader.QueryRowContext(ctx, query, name))
}

// Repositories returns every repository ordered by name.
func (s *SQLStore) Repositories(ctx context.Context) ([]domain.Repository, error) {
	return repositoriesFrom(ctx, s.db)
}

func repositoriesFrom(ctx context.Context, reader classificationReader) ([]domain.Repository, error) {
	query := `SELECT ` + repositoryColumns + ` FROM repositories ORDER BY name`
	rows, err := reader.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	repositories := make([]domain.Repository, 0)
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

// RepositoriesPage reads one bounded name-ordered page.
func (s *SQLStore) RepositoriesPage(ctx context.Context, afterName string, limit int) (RepositoryKeysetPage, error) {
	page := RepositoryKeysetPage{Items: make([]domain.Repository, 0)}
	if limit < 1 || limit > 200 {
		return page, fmt.Errorf("repository page limit must be between 1 and 200")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE name > ? ORDER BY name LIMIT ?`,
		afterName, limit+1,
	)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, repository)
	}
	return page, rows.Err()
}

// PutAsset creates or replaces an asset at a repository-relative path.
func (s *SQLStore) PutAsset(ctx context.Context, asset domain.Asset) (domain.Asset, error) {
	assets, err := s.PutAssets(ctx, []domain.Asset{asset})
	if err != nil {
		return asset, err
	}
	return assets[0], nil
}

// PutAssets publishes a related set of paths in one transaction.
func (s *SQLStore) PutAssets(
	ctx context.Context,
	assets []domain.Asset,
) ([]domain.Asset, error) {
	if len(assets) == 0 {
		return []domain.Asset{}, nil
	}
	now := time.Now().UTC()
	prepared := make([]domain.Asset, len(assets))
	repositories := make(map[string]domain.Repository)
	seen := make(map[string]struct{}, len(assets))
	for index, asset := range assets {
		if !domain.ValidAssetPath(asset.Path) {
			return nil, domain.ErrInvalidAssetPath
		}
		key := asset.Repository + "\x00" + asset.Path
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate asset path %q", domain.ErrConflict, asset.Path)
		}
		seen[key] = struct{}{}
		if asset.Kind == "" {
			asset.Kind = "raw"
		}
		if asset.ContentType == "" {
			asset.ContentType = "application/octet-stream"
		}
		repository, err := s.Repository(ctx, asset.Repository)
		if err != nil {
			return nil, err
		}
		if asset.RepositoryID == "" {
			asset.RepositoryID = repository.ID
		}
		repositories[repository.Name] = repository
		// Record the physical store that receives the bytes. A draining
		// repository binding resolves to its target so it takes no new assets.
		if asset.BlobStore == "" {
			asset.BlobStore, err = s.WriteBlobStore(ctx, repository.BlobStore)
			if err != nil {
				return nil, err
			}
		}
		asset.CreatedAt = now
		asset.UpdatedAt = now
		prepared[index] = asset
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	// Rule updates relabel existing assets under this same lock. Resolve the
	// effective rules after acquiring it so a publication cannot insert labels
	// computed before a completed relabel.
	if err := lockClassificationPublication(ctx, transaction); err != nil {
		return nil, err
	}
	// A row lock cannot protect a path that has not been inserted yet. Lock
	// every publication path before reading existing rows so concurrent first
	// writers cannot both observe absence and let ON CONFLICT replace one. Sort
	// the locks to avoid deadlocks between batches with different input order.
	if s.dialect == dialectPostgres {
		keys := make([]string, 0, len(prepared))
		for _, asset := range prepared {
			keys = append(keys, asset.RepositoryID+"\x00"+asset.Path)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, ownershipLockKey("asset-publication", key)); err != nil {
				return nil, err
			}
		}
	}
	// Proxy fills share one generation authority with negative responses and
	// 304 refreshes. Claim it under this publication transaction before any
	// asset or webhook write, so a delayed older 200 cannot overwrite a newer
	// 200/404 from another replica. The primary asset's token governs the
	// entire batch, including any digest aliases.
	if token := prepared[0].ProxyFetch; token.Epoch != "" {
		claimed, err := claimProxyResultTx(ctx, transaction, prepared[0].RepositoryID, prepared[0].Path, token)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return nil, ErrProxyResultSuperseded
		}
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM negative_cache WHERE repository_id = ? AND path = ?`,
			prepared[0].RepositoryID, prepared[0].Path); err != nil {
			return nil, err
		}
	}
	classifications := make(map[string]domain.ClassificationConfig, len(repositories))
	for _, asset := range prepared {
		config, ok := classifications[asset.Repository]
		if !ok {
			var err error
			config, err = classificationFrom(ctx, transaction, asset.Repository)
			if err != nil {
				return nil, err
			}
			config, err = effectiveClassificationFrom(ctx, transaction, config)
			if err != nil {
				return nil, err
			}
			classifications[asset.Repository] = config
			if repository := repositories[asset.Repository]; repository.Format == "raw" {
				// Raw component rules are classification inputs; read them under
				// the relabel lock so a concurrent rule change cannot be missed.
				var encoded string
				if err := transaction.QueryRowContext(ctx,
					`SELECT format_config FROM repositories WHERE id = ?`, repository.ID,
				).Scan(&encoded); err != nil {
					return nil, err
				}
				if repository.FormatConfig, err = decodeFormatConfig(encoded); err != nil {
					return nil, err
				}
				repositories[asset.Repository] = repository
			}
		}
		if err := s.putAssetTx(ctx, transaction, asset, now); err != nil {
			return nil, err
		}
		// Classify the row after publication has selected its persisted
		// attributes and timestamps. A same-digest publication retains prior
		// annotations and may retain a later download time, while a replacement
		// starts a new asset generation.
		stored, err := scanAsset(transaction.QueryRowContext(ctx,
			`SELECT `+assetColumns+` FROM assets WHERE repository_id = ? AND path = ?`,
			asset.RepositoryID, asset.Path,
		))
		if err != nil {
			return nil, err
		}
		labels := classificationLabels(config, stored, repositories[asset.Repository], now)
		attributes := assetattrs.SetClassificationLabels(stored.Attributes, labels)
		encoded, err := json.Marshal(attributes)
		if err != nil {
			return nil, fmt.Errorf("encode asset attributes: %w", err)
		}
		if _, err := transaction.ExecContext(ctx,
			`UPDATE assets SET attributes = ? WHERE id = ?`, string(encoded), stored.ID,
		); err != nil {
			return nil, err
		}
	}
	// A blob published here resolves the dependency edges that referenced it while
	// it was still absent (a proxy manifest cached before its layers were pulled),
	// so its digest's live pin narrows from every store to this one, atomically
	// with the blob row and under the same publication lease.
	for _, asset := range prepared {
		if asset.Kind != "oci-blob" {
			continue
		}
		if err := resolveAssetDependencyStoreTx(ctx, transaction, asset.BlobStore, asset.Digest); err != nil {
			return nil, err
		}
	}
	// Fan out durable upload deliveries before any asset in this publication
	// becomes visible. A crash can cause a retry/deduplication at the receiver,
	// but cannot leave a committed asset with no scanner event.
	for _, asset := range prepared {
		if asset.Kind == "metadata" {
			continue
		}
		storedAsset, err := scanAsset(transaction.QueryRowContext(
			ctx,
			`SELECT `+assetColumns+` FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`,
			asset.Repository,
			asset.Path,
		))
		if err != nil {
			return nil, err
		}
		if err := s.enqueueAssetUploadedTx(
			ctx, transaction, storedAsset, repositories[asset.Repository], now,
		); err != nil {
			return nil, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	stored := make([]domain.Asset, 0, len(prepared))
	for _, asset := range prepared {
		value, err := s.AssetByRepositoryID(ctx, asset.RepositoryID, asset.Path)
		if err != nil {
			return nil, err
		}
		stored = append(stored, value)
	}
	return stored, nil
}

func (s *SQLStore) putAssetTx(
	ctx context.Context,
	transaction *dialectTx,
	asset domain.Asset,
	now time.Time,
) error {
	attributes, err := json.Marshal(asset.Attributes)
	if err != nil {
		return fmt.Errorf("encode asset attributes: %w", err)
	}
	// Bind the write to one repository identity. Identity-sensitive callers
	// (proxy fetch and cache) pin RepositoryID captured at the start of the
	// operation, so a delete+recreate under the same name cannot redirect a
	// stale write into the replacement: the pinned id is gone and the insert's
	// foreign key fails. Callers that do not pin resolve the current identity.
	repositoryID := asset.RepositoryID
	if repositoryID == "" {
		if err := transaction.QueryRowContext(
			ctx,
			`SELECT id FROM repositories WHERE name = ?`,
			asset.Repository,
		).Scan(&repositoryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
	}
	// Read policy inside this transaction. On PostgreSQL the shared row lock
	// serializes publication with a concurrent administrator update. SQLite
	// serializes writers at the database level.
	var repositoryName, repositoryType, repositoryFormat, repositoryUpstream, repositoryFormatConfig string
	var allowOverwrite bool
	policyQuery := `SELECT name, type, format, upstream, allow_overwrite, format_config FROM repositories WHERE id = ?`
	if s.dialect == dialectPostgres {
		policyQuery += ` FOR SHARE`
	}
	if err := transaction.QueryRowContext(ctx, policyQuery, repositoryID).Scan(
		&repositoryName, &repositoryType, &repositoryFormat, &repositoryUpstream, &allowOverwrite, &repositoryFormatConfig,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	formatConfig, err := decodeFormatConfig(repositoryFormatConfig)
	if err != nil {
		return err
	}
	const query = `
		INSERT INTO assets (
			repository_id, path, format_path, digest, size, blob_store, content_type, kind, reference,
			subject_digest, attributes, created_at, updated_at, validated_at, last_accessed
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repository_id, path) DO UPDATE SET
			format_path = excluded.format_path,
			digest = excluded.digest,
			size = excluded.size,
			blob_store = excluded.blob_store,
			content_type = excluded.content_type,
			kind = excluded.kind,
			reference = excluded.reference,
			subject_digest = excluded.subject_digest,
			attributes = CASE
				WHEN assets.digest = excluded.digest THEN assets.attributes
				ELSE excluded.attributes
			END,
			updated_at = excluded.updated_at,
			validated_at = excluded.validated_at,
			last_accessed = excluded.last_accessed`
	// A path replacement is a new content generation. Give it a new identity
	// so delayed verification/scanner writes addressed to the previous ID
	// cannot annotate the replacement.
	var existingID int64
	var existingDigest string
	var existingLastAccessed sql.NullString
	lastAccessed := now
	existingQuery := `SELECT id, digest, last_accessed FROM assets WHERE repository_id = ? AND path = ?`
	if s.dialect == dialectPostgres {
		existingQuery += ` FOR UPDATE`
	}
	existingErr := transaction.QueryRowContext(
		ctx,
		existingQuery,
		repositoryID,
		asset.Path,
	).Scan(&existingID, &existingDigest, &existingLastAccessed)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return existingErr
	}
	if existingErr == nil && existingDigest != asset.Digest {
		if repositoryType == "hosted" {
			mutableMetadata := asset.Kind == "metadata" && !asset.Immutable
			if registered, found := spiformat.Lookup(repositoryFormat); found {
				view := spiformat.Repository{
					Name: repositoryName, Format: repositoryFormat, Type: repositoryType,
					Upstream: repositoryUpstream, Config: formatConfig, AllowOverwrite: allowOverwrite,
				}
				if policy, ok := registered.(spiformat.MutableHostedPath); ok {
					mutableMetadata = mutableMetadata || policy.MutableHostedPath(
						view, asset.Path)
				}
			}
			digestIdentity := asset.Kind == "oci-blob" ||
				(asset.Kind == "oci-manifest" && strings.HasPrefix(asset.Reference, "sha256:"))
			if digestIdentity || asset.ImmutableIdentity || (!allowOverwrite && !mutableMetadata) {
				return domain.ErrConflict
			}
		} else if asset.Immutable {
			return domain.ErrConflict
		}
		if _, err := transaction.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, existingID); err != nil {
			return err
		}
	} else if existingErr == nil && existingLastAccessed.Valid {
		// A same-digest publication keeps the asset generation. Its start time
		// may precede a download that committed while this write waited for
		// the row, so retain the later recorded access.
		previous, err := parseTime(existingLastAccessed.String)
		if err != nil {
			return err
		}
		if previous.After(lastAccessed) {
			lastAccessed = previous
		}
	}
	_, err = transaction.ExecContext(
		ctx,
		query,
		repositoryID,
		asset.Path,
		asset.FormatPath,
		asset.Digest,
		asset.Size,
		asset.BlobStore,
		asset.ContentType,
		asset.Kind,
		asset.Reference,
		asset.SubjectDigest,
		string(attributes),
		formatTime(now),
		formatTime(now),
		formatTime(now),
		formatTime(lastAccessed),
	)
	if err != nil {
		// A pinned identity that no longer exists (deleted mid-operation) fails
		// the repository_id foreign key; the write must not fall through to the
		// replacement.
		if isRepositoryReferenceError(err) {
			return domain.ErrNotFound
		}
		return err
	}
	var assetID int64
	if err := transaction.QueryRowContext(
		ctx,
		`SELECT id FROM assets WHERE repository_id = ? AND path = ?`,
		repositoryID,
		asset.Path,
	).Scan(&assetID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(
		ctx,
		`DELETE FROM asset_dependencies WHERE asset_id = ?`,
		assetID,
	); err != nil {
		return err
	}
	for _, dependency := range uniqueStrings(asset.Dependencies) {
		if dependency == "" {
			continue
		}
		// Resolve the edge to the depended-on blob's home store when the blob is
		// already published (the direct-push order, where blobs precede their
		// manifest); otherwise the subquery yields NULL and the edge is UNRESOLVED
		// until the blob appears and PutAssets resolves it under the publication
		// lease (the proxy order, where a manifest is cached before its blobs). The
		// store is read from the blob's own asset row, never inferred from this
		// manifest's store, since a proxied manifest's blob may live elsewhere.
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO asset_dependencies (asset_id, digest, blob_store)
			 VALUES (?, ?, (SELECT MIN(blob_store) FROM assets WHERE kind = 'oci-blob' AND digest = ?))`,
			assetID,
			dependency,
			dependency,
		); err != nil {
			return err
		}
	}
	return nil
}

// resolveAssetDependencyStoreTx marks dependency edges on a just-published blob as
// resolved to its home store. An edge stays UNRESOLVED (NULL) until the blob it
// references is published — the state recorded when a manifest is cached before
// its blobs are pulled — at which point the digest's live pin narrows from every
// store to the blob's actual home store, tracked thereafter by the blob's own
// asset row. Only NULL edges are touched, so republishing a digest into another
// store does not repoint edges already resolved elsewhere.
func resolveAssetDependencyStoreTx(ctx context.Context, executor sqlExecer, blobStoreName, digest string) error {
	if digest == "" {
		return nil
	}
	_, err := executor.ExecContext(
		ctx,
		`UPDATE asset_dependencies SET blob_store = ? WHERE digest = ? AND blob_store IS NULL`,
		blobStoreName,
		digest,
	)
	return err
}

// Asset returns one asset by repository and path.
func (s *SQLStore) Asset(
	ctx context.Context,
	repositoryName string,
	assetPath string,
) (domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`
	row := s.db.QueryRowContext(ctx, query, repositoryName, assetPath)
	return scanAsset(row)
}

// AssetByID returns one asset by repository and numeric identifier.
func (s *SQLStore) AssetByID(
	ctx context.Context,
	repositoryName string,
	assetID int64,
) (domain.Asset, error) {
	query := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND id = ?`
	return scanAsset(s.db.QueryRowContext(ctx, query, repositoryName, assetID))
}

// DeleteAsset removes one asset mapping without deleting the underlying blob.
func (s *SQLStore) DeleteAsset(
	ctx context.Context,
	repositoryName string,
	assetPath string,
) (domain.Asset, error) {
	asset, err := s.Asset(ctx, repositoryName, assetPath)
	if err != nil {
		return asset, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, asset.ID); err != nil {
		return asset, err
	}
	return asset, nil
}

// DeleteOCIManifestByDigest atomically removes a canonical manifest and every
// tag that resolves to the same digest in that image namespace. Manifests with
// identical content in another image namespace remain visible.
func (s *SQLStore) DeleteOCIManifestByDigest(
	ctx context.Context,
	repositoryName string,
	manifestPath string,
) ([]domain.Asset, error) {
	marker := strings.LastIndex(manifestPath, "/manifests/")
	if marker < 0 {
		return nil, domain.ErrNotFound
	}
	imagePrefix := manifestPath[:marker+len("/manifests/")]

	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	targetQuery := `SELECT ` + assetColumns + ` FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`
	if s.dialect == dialectPostgres {
		targetQuery += ` FOR UPDATE`
	}
	target, err := scanAsset(transaction.QueryRowContext(
		ctx,
		targetQuery,
		repositoryName,
		manifestPath,
	))
	if err != nil {
		return nil, err
	}
	if target.Kind != "oci-manifest" || !strings.HasPrefix(target.Reference, "sha256:") {
		return nil, domain.ErrNotFound
	}

	rows, err := transaction.QueryContext(
		ctx,
		`DELETE FROM assets
		 WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)
		   AND kind = 'oci-manifest'
		   AND digest = ?
		   AND path LIKE ? ESCAPE '!'
		 RETURNING `+assetColumnsForReturning,
		repositoryName,
		target.Digest,
		escapeLikePrefix(imagePrefix)+"%",
	)
	if err != nil {
		return nil, err
	}
	deleted := make([]domain.Asset, 0)
	for rows.Next() {
		asset, scanErr := scanAssetReturning(rows, repositoryName)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		deleted = append(deleted, asset)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		return nil, domain.ErrNotFound
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

// DeleteAssetIfUnchanged removes an asset only when its digest and update time still match.
func (s *SQLStore) DeleteAssetIfUnchanged(
	ctx context.Context,
	assetID int64,
	digest string,
	updatedAt time.Time,
) (bool, error) {
	const query = `DELETE FROM assets WHERE id = ? AND digest = ? AND updated_at = ?`
	result, err := s.db.ExecContext(ctx, query, assetID, digest, formatTime(updatedAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *SQLStore) DeleteAssetsIfUnchanged(ctx context.Context, assets []domain.Asset) (bool, error) {
	if len(assets) == 0 {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, asset := range assets {
		unchanged, err := deleteAssetIfUnchangedTx(ctx, tx, asset)
		if err != nil {
			return false, err
		}
		if !unchanged {
			return false, nil
		}
	}
	return true, tx.Commit()
}

// DeleteAssetsInDirectoryIfUnchanged protects variable-member format versions.
// The exclusive publication lock makes the directory scan complete on
// PostgreSQL; SQLite's write transaction provides the same serialization.
func (s *SQLStore) DeleteAssetsInDirectoryIfUnchanged(ctx context.Context, directory string, assets []domain.Asset) (bool, error) {
	if len(assets) == 0 || !domain.ValidAssetPath(directory) {
		return false, nil
	}
	repository := assets[0].Repository
	want := make(map[string]int64, len(assets))
	for _, asset := range assets {
		if asset.Repository != repository || !strings.HasPrefix(asset.Path, directory+"/") ||
			strings.Contains(strings.TrimPrefix(asset.Path, directory+"/"), "/") {
			return false, nil
		}
		want[asset.Path] = asset.ID
	}
	if len(want) != len(assets) {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if s.dialect == dialectPostgres {
		if err := lockClassificationRelabel(ctx, tx); err != nil {
			return false, err
		}
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT path, id FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path LIKE ? ESCAPE '!'`,
		repository, escapeLikePrefix(directory+"/")+"%")
	if err != nil {
		return false, err
	}
	seen := 0
	complete := true
	for rows.Next() {
		var path string
		var id int64
		if err := rows.Scan(&path, &id); err != nil {
			rows.Close()
			return false, err
		}
		if !strings.HasPrefix(path, directory+"/") {
			continue
		}
		if strings.Contains(strings.TrimPrefix(path, directory+"/"), "/") {
			continue
		}
		seen++
		if want[path] != id {
			complete = false
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if !complete || seen != len(want) {
		return false, nil
	}
	for _, asset := range assets {
		unchanged, err := deleteAssetIfUnchangedTx(ctx, tx, asset)
		if err != nil {
			return false, err
		}
		if !unchanged {
			return false, nil
		}
	}
	return true, tx.Commit()
}

func (s *SQLStore) DeleteAssetWithCompanions(
	ctx context.Context,
	artifact domain.Asset,
	companionPaths []string,
) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	repositoryID := artifact.RepositoryID
	if repositoryID == "" {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM repositories WHERE name = ?`, artifact.Repository).Scan(&repositoryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, domain.ErrNotFound
			}
			return false, err
		}
	}

	unchanged, err := deleteAssetIfUnchangedTx(ctx, tx, artifact)
	if err != nil {
		return false, err
	}
	if !unchanged {
		return false, nil
	}
	// Delete companions by path within the same transaction, restricted to the
	// metadata kind. Deleting whether or not the companion was observed
	// removes one created concurrently before this deletion; the kind
	// restriction protects a non-metadata asset that shares a declared path.
	for _, companionPath := range companionPaths {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM assets WHERE repository_id = ? AND path = ? AND kind = 'metadata'`,
			repositoryID, companionPath,
		); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// deleteAssetIfUnchangedTx removes one asset within tx only if every stored
// field still matches the observed row. It reports whether the row was
// unchanged (and thus deleted).
func deleteAssetIfUnchangedTx(ctx context.Context, tx *dialectTx, asset domain.Asset) (bool, error) {
	repositoryID := asset.RepositoryID
	if repositoryID == "" {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM repositories WHERE name = ?`, asset.Repository).Scan(&repositoryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, domain.ErrNotFound
			}
			return false, err
		}
	}
	attributes, err := json.Marshal(asset.Attributes)
	if err != nil {
		return false, fmt.Errorf("encode cleanup asset attributes: %w", err)
	}
	var lastAccessed any
	if asset.LastAccessed != nil {
		lastAccessed = formatTime(*asset.LastAccessed)
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM assets WHERE id = ? AND repository_id = ? AND path = ?
		 AND digest = ? AND size = ? AND content_type = ?
		 AND kind = ? AND reference = ? AND subject_digest = ? AND blob_store = ?
		 AND created_at = ? AND updated_at = ?
		 AND COALESCE(validated_at, updated_at) = ? AND attributes = ?
		 AND COALESCE(last_accessed, '') = COALESCE(CAST(? AS TEXT), '')
		`,
		asset.ID, repositoryID, asset.Path, asset.Digest, asset.Size,
		asset.ContentType, asset.Kind, asset.Reference, asset.SubjectDigest, asset.BlobStore,
		formatTime(asset.CreatedAt), formatTime(asset.UpdatedAt),
		formatTime(asset.ValidatedAt), string(attributes), lastAccessed,
	)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

// Assets returns assets whose paths begin with prefix.
func (s *SQLStore) Assets(
	ctx context.Context,
	repositoryName string,
	prefix string,
) ([]domain.Asset, error) {
	query := `SELECT ` + assetColumns + `
        FROM assets
		WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path LIKE ? ESCAPE '!'
        ORDER BY path`
	rows, err := s.db.QueryContext(ctx, query, repositoryName, escapeLikePrefix(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assets := make([]domain.Asset, 0)
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func escapeLikePrefix(value string) string {
	replacer := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return replacer.Replace(value)
}

// ReferencedDigests returns the set of blobs reachable from active assets.
func (s *SQLStore) ReferencedDigests(
	ctx context.Context,
	blobStoreName string,
) (map[string]struct{}, error) {
	return s.referencedDigests(ctx, blobStoreName, true)
}

// PublishedReferencedDigests omits unresolved OCI dependencies. Those edges
// retain future blobs for GC, but no store is expected to contain their bytes
// until an oci-blob asset has been published.
func (s *SQLStore) PublishedReferencedDigests(
	ctx context.Context,
	blobStoreName string,
) (map[string]struct{}, error) {
	return s.referencedDigests(ctx, blobStoreName, false)
}

func (s *SQLStore) referencedDigests(
	ctx context.Context,
	blobStoreName string,
	includePending bool,
) (map[string]struct{}, error) {
	// The dependency arms pin a depended-on blob two ways. A blob that exists is
	// covered by the join to its own oci-blob row, which pins it in whatever store
	// currently holds it (even after a drain). A dependency whose blob is not
	// published anywhere has no row to join, so the third arm pins its digest in
	// every store, which is how a proxy manifest cached before its layers keeps
	// them from being collected once they appear.
	//
	// The third arm is gated on there being no oci-blob row for the digest, not on
	// the edge's blob_store being NULL alone: an edge can stay NULL after its blob
	// is published (an old binary that does not resolve, or a publication-order
	// race), and pinning every store on a NULL that is really resolved would
	// over-retain unrelated stores forever. Gating on the row's absence converges
	// as soon as the blob exists — the join arm then pins its real store — so GC
	// correctness never depends on the edge itself being resolved. blob_store IS
	// NULL is kept so a resolved edge whose blob was later deleted is not treated
	// as pending here (the join arm already governs a deleted blob, as before).
	query := `
		SELECT DISTINCT assets.digest
		FROM assets
		WHERE assets.kind <> 'oci-blob' AND assets.blob_store = ?
		UNION
		SELECT DISTINCT blobs.digest
		FROM asset_dependencies
		JOIN assets AS blobs
			ON blobs.kind = 'oci-blob'
			AND blobs.digest = asset_dependencies.digest
		WHERE blobs.blob_store = ?`
	if includePending {
		query += `
			UNION
			SELECT DISTINCT asset_dependencies.digest
			FROM asset_dependencies
			WHERE asset_dependencies.blob_store IS NULL
				AND NOT EXISTS (
					SELECT 1 FROM assets AS existing
					WHERE existing.kind = 'oci-blob'
						AND existing.digest = asset_dependencies.digest
				)`
	}
	rows, err := s.db.QueryContext(ctx, query, blobStoreName, blobStoreName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	digests := make(map[string]struct{})
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, err
		}
		digests[digest] = struct{}{}
	}
	return digests, rows.Err()
}

// DeleteUnreferencedBlobAssets removes OCI blob metadata with no manifest dependency.
func (s *SQLStore) DeleteUnreferencedBlobAssets(
	ctx context.Context,
	blobStoreName string,
	digest string,
) (int64, error) {
	const query = `
		DELETE FROM assets
		WHERE kind = 'oci-blob'
			AND digest = ?
			AND blob_store = ?
			AND NOT EXISTS (
				SELECT 1
				FROM asset_dependencies
				WHERE asset_dependencies.digest = ?
			)`
	result, err := s.db.ExecContext(
		ctx,
		query,
		digest,
		blobStoreName,
		digest,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteDanglingManifestAliases prunes canonical aliases released by removed tags.
func (s *SQLStore) DeleteDanglingManifestAliases(
	ctx context.Context,
	repositoryName string,
	removed []domain.Asset,
) (int64, error) {
	queue := make([]string, 0)
	for _, asset := range removed {
		if asset.Kind != "oci-manifest" || strings.HasPrefix(asset.Reference, "sha256:") {
			continue
		}
		route, ok := ocimodel.ParseAssetPath(asset.Path)
		if !ok || route.Kind != ocimodel.ManifestRoute {
			continue
		}
		queue = append(queue, ocimodel.ManifestPath(route.ImageName, asset.Digest))
	}
	if len(queue) == 0 {
		return 0, nil
	}
	repositoryID := ""
	for _, asset := range removed {
		if asset.RepositoryID == "" {
			continue
		}
		if repositoryID != "" && repositoryID != asset.RepositoryID {
			return 0, domain.ErrConflict
		}
		repositoryID = asset.RepositoryID
	}
	if repositoryID == "" {
		repository, err := s.Repository(ctx, repositoryName)
		if err != nil {
			return 0, err
		}
		repositoryID = repository.ID
	}

	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	var deleted int64
	visited := make(map[string]struct{})
	for len(queue) > 0 {
		candidatePath := queue[0]
		queue = queue[1:]
		if _, found := visited[candidatePath]; found {
			continue
		}
		visited[candidatePath] = struct{}{}

		var assetID int64
		var storedDigest string
		// A concurrent tag publication writes its tag before upserting this
		// canonical alias. Lock the alias before counting committed tag
		// references; otherwise a publisher can commit after the count but
		// before our DELETE, leaving its new tag without a digest alias.
		candidateQuery := `SELECT id, digest FROM assets WHERE repository_id = ? AND path = ?`
		if s.dialect == dialectPostgres {
			candidateQuery += ` FOR UPDATE`
		}
		err := transaction.QueryRowContext(
			ctx,
			candidateQuery,
			repositoryID,
			candidatePath,
		).Scan(&assetID, &storedDigest)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return deleted, err
		}
		route, ok := ocimodel.ParseAssetPath(candidatePath)
		if !ok || route.Kind != ocimodel.ManifestRoute {
			continue
		}
		var tagReferences int
		const tagsQuery = `
			SELECT COUNT(*)
			FROM assets
			WHERE repository_id = ?
				AND kind = 'oci-manifest'
				AND reference NOT LIKE 'sha256:%'
				AND digest = ?
				AND path LIKE ? ESCAPE '!'`
		if err := transaction.QueryRowContext(
			ctx,
			tagsQuery,
			repositoryID,
			storedDigest,
			escapeLikePrefix(ocimodel.ManifestPath(route.ImageName, ""))+"%",
		).Scan(&tagReferences); err != nil {
			return deleted, err
		}
		if tagReferences > 0 {
			continue
		}
		var inboundReferences int
		if err := transaction.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM asset_dependencies WHERE digest = ? AND asset_id <> ?`,
			storedDigest,
			assetID,
		).Scan(&inboundReferences); err != nil {
			return deleted, err
		}
		if inboundReferences > 0 {
			continue
		}

		dependencies, err := queryStrings(
			ctx,
			transaction,
			`SELECT digest FROM asset_dependencies WHERE asset_id = ?`,
			assetID,
		)
		if err != nil {
			return deleted, err
		}
		if _, err := transaction.ExecContext(
			ctx,
			`DELETE FROM assets WHERE id = ?`,
			assetID,
		); err != nil {
			return deleted, err
		}
		deleted++
		for _, dependency := range dependencies {
			rows, err := transaction.QueryContext(
				ctx,
				`SELECT path FROM assets
				 WHERE repository_id = ? AND kind = 'oci-manifest'
				 AND reference LIKE 'sha256:%' AND digest = ?`,
				repositoryID,
				dependency,
			)
			if err != nil {
				return deleted, err
			}
			for rows.Next() {
				var childPath string
				if err := rows.Scan(&childPath); err != nil {
					rows.Close()
					return deleted, err
				}
				queue = append(queue, childPath)
			}
			if err := rows.Close(); err != nil {
				return deleted, err
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// NegativeCacheHit reports whether a repository path has an unexpired not-found entry.
func (s *SQLStore) NegativeCacheHit(
	ctx context.Context,
	repositoryName string,
	assetPath string,
	now time.Time,
) (bool, error) {
	const query = `
        SELECT expires_at
        FROM negative_cache
        WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`
	var expiresAtValue string
	err := s.db.QueryRowContext(ctx, query, repositoryName, assetPath).Scan(&expiresAtValue)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expiresAt, err := parseTime(expiresAtValue)
	if err != nil {
		return false, err
	}
	if !expiresAt.After(now) {
		// Another fetch may have renewed this entry after the read. Remove only
		// the expiry observed here, never a newer result.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM negative_cache
			WHERE repository_id = (SELECT id FROM repositories WHERE name = ?)
			AND path = ? AND expires_at = ?`, repositoryName, assetPath, expiresAtValue)
		return false, nil
	}
	return true, nil
}

// PutNegativeCacheByRepositoryID records a not-found response until its expiry
// time. It is keyed on the repository's internal id captured at the start of the
// fetch, so a delete+recreate under the same name cannot inherit the stale entry:
// the pinned id is gone and the foreign key rejects the write.
func (s *SQLStore) PutNegativeCacheByRepositoryID(
	ctx context.Context,
	repositoryID string,
	assetPath string,
	expiresAt time.Time,
) error {
	const query = `
        INSERT INTO negative_cache (repository_id, path, expires_at)
        VALUES (?, ?, ?)
        ON CONFLICT(repository_id, path) DO UPDATE SET expires_at = excluded.expires_at`
	_, err := s.db.ExecContext(
		ctx,
		query,
		repositoryID,
		assetPath,
		formatTime(expiresAt),
	)
	if isRepositoryReferenceError(err) {
		return domain.ErrNotFound
	}
	return err
}

// ClearNegativeCache removes a cached not-found response.
func (s *SQLStore) ClearNegativeCache(
	ctx context.Context,
	repositoryName string,
	assetPath string,
) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM negative_cache WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`,
		repositoryName,
		assetPath,
	)
	return err
}

// TouchAsset records a successful access (a download) time.
func (s *SQLStore) TouchAsset(ctx context.Context, assetID int64, accessedAt time.Time) error {
	return s.touchAsset(ctx, "", assetID, accessedAt)
}

// touchAsset serializes access timestamps for one row. Downloads update this
// asynchronously, so their writes can arrive out of order. Comparing parsed
// times also handles the variable fractional width of stored RFC3339Nano values.
func (s *SQLStore) touchAsset(ctx context.Context, repositoryID string, assetID int64, accessedAt time.Time) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	where := ` WHERE id = ?`
	arguments := []any{assetID}
	if repositoryID != "" {
		where += ` AND repository_id = ?`
		arguments = append(arguments, repositoryID)
	}
	query := `SELECT last_accessed FROM assets` + where
	if s.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	var current sql.NullString
	if err := transaction.QueryRowContext(ctx, query, arguments...).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if current.Valid {
		previous, err := parseTime(current.String)
		if err != nil {
			return err
		}
		if !accessedAt.After(previous) {
			return transaction.Commit()
		}
	}
	_, err = transaction.ExecContext(ctx, `UPDATE assets SET last_accessed = ?`+where,
		append([]any{formatTime(accessedAt)}, arguments...)...)
	if err != nil {
		return err
	}
	return transaction.Commit()
}

// RefreshAsset advances the validation time for an unchanged proxy asset.
func (s *SQLStore) RefreshAsset(ctx context.Context, assetID int64, validatedAt time.Time) error {
	const query = `UPDATE assets SET validated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, formatTime(validatedAt), assetID)
	return err
}

// SetAttributes replaces one namespaced attribute value on an asset. HTTP/API
// callers enforce reserved namespaces before invoking this trusted primitive.
func (s *SQLStore) SetAttributes(
	ctx context.Context,
	repositoryName string,
	assetID int64,
	namespace string,
	value map[string]any,
) error {
	for attempt := 0; attempt < 8; attempt++ {
		var encoded string
		err := s.db.QueryRowContext(
			ctx,
			`SELECT attributes FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND id = ?`,
			repositoryName,
			assetID,
		).Scan(&encoded)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		attributes := make(map[string]any)
		if err := decodeJSONNumbers(encoded, &attributes); err != nil {
			return fmt.Errorf("decode asset attributes: %w", err)
		}
		attributes[namespace] = value
		updated, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("encode asset attributes: %w", err)
		}
		result, err := s.db.ExecContext(
			ctx,
			`UPDATE assets SET attributes = ? WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND id = ? AND attributes = ?`,
			string(updated), repositoryName, assetID, encoded,
		)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 1 {
			return nil
		}
	}
	return domain.ErrConflict
}

// DeleteAttributes removes one namespaced attribute value from an asset. HTTP/API
// callers enforce reserved namespaces before invoking this trusted primitive.
func (s *SQLStore) DeleteAttributes(
	ctx context.Context,
	repositoryName string,
	assetID int64,
	namespace string,
) error {
	for attempt := 0; attempt < 8; attempt++ {
		var encoded string
		err := s.db.QueryRowContext(
			ctx,
			`SELECT attributes FROM assets WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND id = ?`,
			repositoryName,
			assetID,
		).Scan(&encoded)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		attributes := make(map[string]any)
		if err := decodeJSONNumbers(encoded, &attributes); err != nil {
			return fmt.Errorf("decode asset attributes: %w", err)
		}
		if _, found := attributes[namespace]; !found {
			return domain.ErrNotFound
		}
		delete(attributes, namespace)
		updated, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("encode asset attributes: %w", err)
		}
		result, err := s.db.ExecContext(
			ctx,
			`UPDATE assets SET attributes = ? WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND id = ? AND attributes = ?`,
			string(updated), repositoryName, assetID, encoded,
		)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 1 {
			return nil
		}
	}
	return domain.ErrConflict
}

// CreateUser inserts a local account with an Argon2id password hash.
func (s *SQLStore) CreateUser(
	ctx context.Context,
	username string,
	password string,
	admin bool,
) error {
	if !domain.ValidUsername(username) {
		return domain.ErrInvalidUsername
	}
	if password == "" {
		return domain.ErrPasswordRequired
	}
	encodedPassword, err := hashPassword(password)
	if err != nil {
		return err
	}
	identity, err := newUserIdentity()
	if err != nil {
		return err
	}

	const query = `
        INSERT INTO users (username, password_hash, admin, created_at, identity)
        VALUES (?, ?, ?, ?, ?)`
	_, err = s.db.ExecContext(
		ctx,
		query,
		username,
		encodedPassword,
		admin,
		formatTime(time.Now()),
		identity,
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	return s.invalidateProvisionSecret(ctx, "user", username)
}

// User returns one local account without credential material.
func (s *SQLStore) User(ctx context.Context, username string) (domain.User, error) {
	const query = `SELECT username, admin, created_at, identity FROM users WHERE username = ?`
	var user domain.User
	var createdAt string
	err := s.db.QueryRowContext(ctx, query, username).Scan(
		&user.Username,
		&user.Admin,
		&createdAt,
		&user.Identity,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return user, domain.ErrNotFound
	}
	if err != nil {
		return user, err
	}
	user.CreatedAt, err = parseTime(createdAt)
	return user, err
}

// Users returns local accounts without credential material.
func (s *SQLStore) Users(ctx context.Context) ([]domain.User, error) {
	const query = `SELECT username, admin, created_at, identity FROM users ORDER BY username`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]domain.User, 0)
	for rows.Next() {
		var user domain.User
		var createdAt string
		if err := rows.Scan(&user.Username, &user.Admin, &createdAt, &user.Identity); err != nil {
			return nil, err
		}
		user.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// UpdateUser changes administrator status and optionally replaces the password.
func (s *SQLStore) UpdateUser(
	ctx context.Context,
	username string,
	password string,
	admin bool,
) error {
	if !domain.ValidUsername(username) {
		return domain.ErrInvalidUsername
	}
	if password == "" {
		result, err := s.db.ExecContext(
			ctx,
			`UPDATE users SET admin = ? WHERE username = ?`,
			admin,
			username,
		)
		if err != nil {
			return err
		}
		return requireAffectedRow(result)
	}
	var existingPasswordHash string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT password_hash FROM users WHERE username = ?`,
		username,
	).Scan(&existingPasswordHash); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if verifyPassword(existingPasswordHash, password) {
		result, err := s.db.ExecContext(
			ctx,
			`UPDATE users SET admin = ? WHERE username = ?`,
			admin,
			username,
		)
		if err != nil {
			return err
		}
		return requireAffectedRow(result)
	}

	encodedPassword, err := hashPassword(password)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE users SET password_hash = ?, admin = ? WHERE username = ?`,
		encodedPassword,
		admin,
		username,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return err
	}
	return s.invalidateProvisionSecret(ctx, "user", username)
}

func secretValuesEqual(left string, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// DeleteUser removes a local account, its role assignments, API tokens, and its
// ownership record in one transaction serialized on ("user", name). An
// imperative delete of a provisioning-managed account returns domain.ErrManaged
// unless the ownership carries Force.
func (s *SQLStore) DeleteUser(ctx context.Context, username string, ownership Ownership) error {
	return s.writeOwned(ctx, "user", username, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

// AuthenticatePassword validates a local username and password.
func (s *SQLStore) AuthenticatePassword(
	ctx context.Context,
	username string,
	password string,
) (domain.User, bool) {
	const query = `SELECT username, password_hash, admin, created_at, identity FROM users WHERE username = ?`
	var user domain.User
	var encodedPassword string
	var createdAt string
	err := s.db.QueryRowContext(ctx, query, username).Scan(
		&user.Username,
		&encodedPassword,
		&user.Admin,
		&createdAt,
		&user.Identity,
	)
	if err != nil || !verifyPassword(encodedPassword, password) {
		return domain.User{}, false
	}
	user.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.User{}, false
	}
	return user, true
}

// CreateToken stores a one-way hash of an API bearer token.
func (s *SQLStore) CreateToken(
	ctx context.Context,
	username string,
	name string,
	token string,
	scopes []string,
) (domain.APIToken, error) {
	created := domain.APIToken{
		Username:  username,
		Name:      name,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}
	tokenHash := sha256.Sum256([]byte(token))
	encodedScopes, err := json.Marshal(scopes)
	if err != nil {
		return created, fmt.Errorf("encode token scopes: %w", err)
	}
	const query = `
        INSERT INTO tokens (username, name, token_hash, scopes, created_at)
        VALUES (?, ?, ?, ?, ?)
		RETURNING id`
	err = s.db.QueryRowContext(
		ctx,
		query,
		username,
		name,
		hex.EncodeToString(tokenHash[:]),
		string(encodedScopes),
		formatTime(created.CreatedAt),
	).Scan(&created.ID)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		// A user can be deleted after the API checks it but before this insert.
		// The username reference is the only foreign key on a token row.
		return created, domain.ErrNotFound
	}
	return created, err
}

// AuthenticateToken validates an API bearer token.
func (s *SQLStore) AuthenticateToken(ctx context.Context, token string) (domain.User, bool) {
	tokenHash := sha256.Sum256([]byte(token))
	const query = `
		SELECT users.username, users.admin, users.created_at, users.identity, tokens.scopes
        FROM tokens
        JOIN users ON users.username = tokens.username
        WHERE tokens.token_hash = ?`

	var user domain.User
	var createdAt string
	var encodedScopes string
	err := s.db.QueryRowContext(ctx, query, hex.EncodeToString(tokenHash[:])).Scan(
		&user.Username,
		&user.Admin,
		&createdAt,
		&user.Identity,
		&encodedScopes,
	)
	if err != nil {
		return domain.User{}, false
	}
	user.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.User{}, false
	}
	if err := json.Unmarshal([]byte(encodedScopes), &user.TokenScopes); err != nil {
		return domain.User{}, false
	}
	return user, true
}

// Tokens returns token metadata for a user without token secrets or hashes.
func (s *SQLStore) Tokens(ctx context.Context, username string) ([]domain.APIToken, error) {
	const query = `
        SELECT id, username, name, scopes, created_at
        FROM tokens
        WHERE username = ?
        ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := make([]domain.APIToken, 0)
	for rows.Next() {
		var token domain.APIToken
		var encodedScopes string
		var createdAt string
		if err := rows.Scan(
			&token.ID,
			&token.Username,
			&token.Name,
			&encodedScopes,
			&createdAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedScopes), &token.Scopes); err != nil {
			return nil, fmt.Errorf("decode token scopes: %w", err)
		}
		token.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// DeleteToken revokes one token owned by a user.
func (s *SQLStore) DeleteToken(ctx context.Context, username string, tokenID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM tokens WHERE username = ? AND id = ?`,
		username,
		tokenID,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

// Stats returns current repository and content counters.
func (s *SQLStore) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	queries := []struct {
		query       string
		destination *int64
	}{
		{`SELECT COUNT(*) FROM repositories`, &stats.Repositories},
		{`SELECT COUNT(*) FROM assets`, &stats.Assets},
		{
			`SELECT COUNT(*)
			 FROM (
				 SELECT assets.blob_store, assets.digest
				 FROM assets
				 GROUP BY assets.blob_store, assets.digest
			 )`,
			&stats.UniqueBlobs,
		},
		{
			`SELECT COALESCE(SUM(size), 0)
			 FROM (
				 SELECT assets.blob_store, assets.digest, MAX(assets.size) AS size
				 FROM assets
				 GROUP BY assets.blob_store, assets.digest
			 )`,
			&stats.Bytes,
		},
		{
			`SELECT COUNT(*) FROM webhook_deliveries
			 WHERE status IN ('queued', 'retry', 'delivering')`,
			&stats.WebhookQueue,
		},
		{
			`SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'dead'`,
			&stats.WebhookDead,
		},
	}

	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.destination); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// StorageUsage reports deduplicated blob bytes per repository and per blob store.
// Both dedup by digest within their group: repository totals are the logical
// footprint (a shared blob counts toward each repository referencing it), blob
// store totals the physical footprint keyed on the asset's own home store.
func (s *SQLStore) StorageUsage(ctx context.Context) (StorageUsage, error) {
	// Non-nil slices so the JSON response is always an array, never null.
	usage := StorageUsage{
		Repositories: []RepositoryUsage{},
		BlobStores:   []BlobStoreUsage{},
	}
	repositories, err := s.groupedUsage(
		ctx,
		`SELECT r.name, COALESCE(SUM(u.size), 0), COUNT(*)
		 FROM (
			 SELECT repository_id, digest, MAX(size) AS size
			 FROM assets
			 GROUP BY repository_id, digest
		 ) u
		 JOIN repositories r ON r.id = u.repository_id
		 GROUP BY r.name`,
	)
	if err != nil {
		return usage, err
	}
	for _, row := range repositories {
		usage.Repositories = append(usage.Repositories, RepositoryUsage{
			Repository: row.name,
			Blobs:      row.blobs,
			Bytes:      row.bytes,
		})
	}
	blobStores, err := s.groupedUsage(
		ctx,
		`SELECT blob_store, COALESCE(SUM(size), 0), COUNT(*)
		 FROM (
			 SELECT blob_store, digest, MAX(size) AS size
			 FROM assets
			 GROUP BY blob_store, digest
		 )
		 GROUP BY blob_store`,
	)
	if err != nil {
		return usage, err
	}
	for _, row := range blobStores {
		usage.BlobStores = append(usage.BlobStores, BlobStoreUsage{
			BlobStore: row.name,
			Blobs:     row.blobs,
			Bytes:     row.bytes,
		})
	}
	return usage, nil
}

type usageRow struct {
	name  string
	bytes int64
	blobs int64
}

func (s *SQLStore) groupedUsage(ctx context.Context, query string) ([]usageRow, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []usageRow
	for rows.Next() {
		var row usageRow
		if err := rows.Scan(&row.name, &row.bytes, &row.blobs); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

type scanner interface {
	Scan(destinations ...any) error
}

func scanRepository(source scanner) (domain.Repository, error) {
	var repository domain.Repository
	var members string
	var formatConfig string
	var endpoints string
	var createdAt string
	var repositoryAllowOverwrite bool

	err := source.Scan(
		&repository.ID,
		&repository.Name,
		&repository.Format,
		&repository.Type,
		&repository.BlobStore,
		&repository.Upstream,
		&members,
		&repository.Writable,
		&repositoryAllowOverwrite,
		&formatConfig,
		&endpoints,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return repository, domain.ErrNotFound
	}
	if err != nil {
		return repository, err
	}
	if repository.Type == "hosted" {
		repository.AllowOverwrite = &repositoryAllowOverwrite
	}
	if err := json.Unmarshal([]byte(members), &repository.Members); err != nil {
		return repository, fmt.Errorf("decode repository members: %w", err)
	}
	repository.FormatConfig, err = decodeFormatConfig(formatConfig)
	if err != nil {
		return repository, err
	}
	repository.Endpoints, err = decodeRepositoryEndpoints(endpoints)
	if err != nil {
		return repository, err
	}
	repository.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return repository, err
	}
	return repository, nil
}

// encodeFormatConfig stores format-owned settings as a JSON object; an empty
// configuration round-trips to nil so responses omit it.
func encodeFormatConfig(config map[string]any) (string, error) {
	if len(config) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode repository format config: %w", err)
	}
	return string(encoded), nil
}

func decodeFormatConfig(encoded string) (map[string]any, error) {
	if encoded == "" || encoded == "{}" {
		return nil, nil
	}
	var config map[string]any
	if err := decodeJSONNumbers(encoded, &config); err != nil {
		return nil, fmt.Errorf("decode repository format config: %w", err)
	}
	if len(config) == 0 {
		return nil, nil
	}
	return config, nil
}

func encodeRepositoryEndpoints(endpoints *domain.RepositoryEndpoints) (string, error) {
	if endpoints.Empty() {
		return "{}", nil
	}
	encoded, err := json.Marshal(endpoints)
	if err != nil {
		return "", fmt.Errorf("encode repository endpoints: %w", err)
	}
	return string(encoded), nil
}

func decodeRepositoryEndpoints(encoded string) (*domain.RepositoryEndpoints, error) {
	if encoded == "" || encoded == "{}" {
		return nil, nil
	}
	var endpoints domain.RepositoryEndpoints
	if err := json.Unmarshal([]byte(encoded), &endpoints); err != nil {
		return nil, fmt.Errorf("decode repository endpoints: %w", err)
	}
	if endpoints.Empty() {
		return nil, nil
	}
	return &endpoints, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *SQLStore) rejectConflictingOCIEndpoints(
	ctx context.Context,
	source queryer,
	repository domain.Repository,
) error {
	if repository.Endpoints.Empty() {
		return nil
	}
	rows, err := source.QueryContext(ctx, `SELECT name, endpoints FROM repositories`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var encoded string
		if err := rows.Scan(&name, &encoded); err != nil {
			return err
		}
		if name == repository.Name {
			continue
		}
		existing, err := decodeRepositoryEndpoints(encoded)
		if err != nil {
			return err
		}
		if domain.OCIEndpointsOverlap(repository.Endpoints, existing) {
			return domain.ErrOCIEndpointConflict
		}
	}
	return rows.Err()
}

func scanAsset(source scanner) (domain.Asset, error) {
	var asset domain.Asset
	var attributes string
	var createdAt string
	var updatedAt string
	var validatedAt sql.NullString
	var lastAccessed sql.NullString

	err := source.Scan(
		&asset.ID,
		&asset.Repository,
		&asset.Path,
		&asset.FormatPath,
		&asset.Digest,
		&asset.Size,
		&asset.BlobStore,
		&asset.ContentType,
		&asset.Kind,
		&asset.Reference,
		&asset.SubjectDigest,
		&attributes,
		&createdAt,
		&updatedAt,
		&validatedAt,
		&lastAccessed,
	)
	return finishAsset(asset, err, attributes, createdAt, updatedAt, validatedAt, lastAccessed)
}

// scanAssetReturning scans a row from a DELETE ... RETURNING assetColumnsForReturning,
// whose second column is the raw repository_id; the caller supplies the known
// repository name because the deletion is scoped to a single repository.
func scanAssetReturning(source scanner, repositoryName string) (domain.Asset, error) {
	var asset domain.Asset
	var repositoryID string
	var attributes string
	var createdAt string
	var updatedAt string
	var validatedAt sql.NullString
	var lastAccessed sql.NullString

	err := source.Scan(
		&asset.ID,
		&repositoryID,
		&asset.Path,
		&asset.FormatPath,
		&asset.Digest,
		&asset.Size,
		&asset.BlobStore,
		&asset.ContentType,
		&asset.Kind,
		&asset.Reference,
		&asset.SubjectDigest,
		&attributes,
		&createdAt,
		&updatedAt,
		&validatedAt,
		&lastAccessed,
	)
	asset.Repository = repositoryName
	return finishAsset(asset, err, attributes, createdAt, updatedAt, validatedAt, lastAccessed)
}

func finishAsset(
	asset domain.Asset,
	err error,
	attributes string,
	createdAt string,
	updatedAt string,
	validatedAt sql.NullString,
	lastAccessed sql.NullString,
) (domain.Asset, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return asset, domain.ErrNotFound
	}
	if err != nil {
		return asset, err
	}
	if err := decodeJSONNumbers(attributes, &asset.Attributes); err != nil {
		return asset, fmt.Errorf("decode asset attributes: %w", err)
	}
	asset.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return asset, err
	}
	asset.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return asset, err
	}
	asset.ValidatedAt = asset.UpdatedAt
	if validatedAt.Valid {
		asset.ValidatedAt, err = parseTime(validatedAt.String)
		if err != nil {
			return asset, err
		}
	}
	if lastAccessed.Valid {
		accessedAt, err := parseTime(lastAccessed.String)
		if err != nil {
			return asset, err
		}
		asset.LastAccessed = &accessedAt
	}
	return asset, nil
}

func repositoryAllowOverwrite(repository domain.Repository) bool {
	return repository.AllowOverwrite != nil && *repository.AllowOverwrite
}

func normalizeRepository(repository *domain.Repository) {
	normalizeRepositoryBase(repository)
	if repository.Type == "hosted" && repository.AllowOverwrite == nil {
		allow := domain.DefaultAllowOverwrite(repository.Format)
		repository.AllowOverwrite = &allow
	}
}

func normalizeRepositoryBase(repository *domain.Repository) {
	if repository.BlobStore == "" {
		repository.BlobStore = "default"
	}
	repository.Writable = repository.Type == "hosted"
	if repository.CreatedAt.IsZero() {
		repository.CreatedAt = time.Now().UTC()
	}
	if repository.Endpoints == nil {
		return
	}
	for index, host := range repository.Endpoints.Hosts {
		canonical, err := domain.CanonicalOCIEndpointHost(host)
		if err == nil {
			repository.Endpoints.Hosts[index] = canonical
		}
	}
	if repository.Endpoints.Empty() {
		repository.Endpoints = nil
	}
}

func requireAffectedRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	hash := argon2.IDKey(
		[]byte(password),
		salt,
		passwordIterations,
		passwordMemory,
		passwordThreads,
		passwordKeyBytes,
	)

	parameters := fmt.Sprintf(
		"m=%d,t=%d,p=%d",
		passwordMemory,
		passwordIterations,
		passwordThreads,
	)
	return strings.Join([]string{
		"",
		passwordAlgorithm,
		"v=19",
		parameters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	}, "$"), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != passwordAlgorithm || parts[2] != "v=19" {
		return false
	}

	var memory uint32
	var iterations uint32
	var threads uint8
	_, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads)
	if err != nil || !validArgon2Parameters(memory, iterations, threads) {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	wanted, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	actual := argon2.IDKey(
		[]byte(password),
		salt,
		iterations,
		memory,
		threads,
		uint32(len(wanted)),
	)
	return subtle.ConstantTimeCompare(wanted, actual) == 1
}

func validArgon2Parameters(memory, iterations uint32, threads uint8) bool {
	return memory >= 8*1024 && memory <= 256*1024 &&
		iterations >= 1 && iterations <= 10 &&
		threads >= 1 && threads <= 16
}

func formatTime(value time.Time) string {
	// TEXT timestamps need a fixed fractional width for SQL comparisons and
	// ORDER BY to agree with chronological order.
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse database timestamp: %w", err)
	}
	return parsed, nil
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func isBlobStoreReferenceError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknown blob store") ||
		strings.Contains(message, "repositories_blob_store_foreign_key")
}

// isRepositoryReferenceError reports whether err is the repository_id foreign
// key failing on an asset write, which happens when the pinned repository
// identity was deleted mid-operation. It is the only foreign key an asset
// insert exercises.
func isRepositoryReferenceError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "foreign key") ||
		strings.Contains(message, "assets_repository_id_fkey")
}

func isRepositoryBlobStoreInUseError(err error) bool {
	return err != nil && strings.Contains(
		strings.ToLower(err.Error()),
		"repository blob store in use",
	)
}
