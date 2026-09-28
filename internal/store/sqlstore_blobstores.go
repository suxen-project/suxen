package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

const blobStoreColumns = `
    name,
    driver,
    configuration_env,
    configuration_file,
    physical_identity,
    attributes,
    state,
    drain_target,
    created_at`

// CreateBlobStore inserts a named blob storage backend.
func (s *SQLStore) CreateBlobStore(ctx context.Context, blobStore domain.BlobStore) error {
	if err := blobStore.Validate(); err != nil {
		return err
	}
	normalizeBlobStore(&blobStore)
	return insertBlobStoreTx(ctx, s.db, blobStore)
}

// BlobStoreSave is an atomic blob-store mutation and its ownership effect.
// Create inserts a new backend; an update replaces the editable attributes in
// place.
type BlobStoreSave struct {
	BlobStore domain.BlobStore
	Create    bool
	Ownership Ownership
}

// SaveBlobStore commits a blob-store row and its ownership record in one
// transaction serialized on ("blobStore", name). Physical backend readiness and
// the in-memory backend cache are the caller's concern: this method touches only
// metadata, so the caller verifies the backend before a create and remembers it
// after commit. An imperative mutation of a provisioning-managed blob store
// returns domain.ErrManaged unless Force is set.
func (s *SQLStore) SaveBlobStore(ctx context.Context, save BlobStoreSave) error {
	blobStore := save.BlobStore
	if err := blobStore.Validate(); err != nil {
		return err
	}
	normalizeBlobStore(&blobStore)
	return s.writeOwned(ctx, "blobStore", blobStore.Name, save.Ownership, !save.Create, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if save.Create {
				return insertBlobStoreTx(ctx, transaction, blobStore)
			}
			return s.updateBlobStoreTx(ctx, transaction, blobStore)
		})
}

// insertBlobStoreTx inserts a blob-store row using the given executor, which is
// the store's connection for a standalone create or a transaction for the
// ownership-folded SaveBlobStore.
func insertBlobStoreTx(ctx context.Context, executor sqlExecer, blobStore domain.BlobStore) error {
	configurationEnvironment, configurationFile := configurationReferenceColumns(blobStore)
	attributes, err := json.Marshal(blobStore.Attributes)
	if err != nil {
		return fmt.Errorf("encode blob store attributes: %w", err)
	}
	const query = `
        INSERT INTO blob_stores (
			name, driver, configuration_env, configuration_file,
			physical_identity, attributes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err = executor.ExecContext(
		ctx,
		query,
		blobStore.Name,
		blobStore.Driver,
		configurationEnvironment,
		configurationFile,
		blobStore.PhysicalIdentity,
		string(attributes),
		formatTime(blobStore.CreatedAt),
	)
	if isBlobStoreIdentityConstraint(err) {
		return domain.ErrBlobStoreIdentityConflict
	}
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

// UpdateBlobStore replaces a named blob storage backend while preserving its name.
func (s *SQLStore) UpdateBlobStore(ctx context.Context, blobStore domain.BlobStore) error {
	if err := blobStore.Validate(); err != nil {
		return err
	}
	normalizeBlobStore(&blobStore)
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.updateBlobStoreTx(ctx, transaction, blobStore); err != nil {
		return err
	}
	return transaction.Commit()
}

// updateBlobStoreTx replaces a blob store's editable attributes within a caller's
// transaction. The driver, configuration reference and physical destination are
// immutable; an identical definition is a no-op.
func (s *SQLStore) updateBlobStoreTx(ctx context.Context, transaction *dialectTx, blobStore domain.BlobStore) error {
	query := `SELECT ` + blobStoreColumns + ` FROM blob_stores WHERE name = ?`
	if s.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	existing, err := scanBlobStore(transaction.QueryRowContext(ctx, query, blobStore.Name))
	if err != nil {
		return err
	}
	if blobStoreDefinitionEqual(existing, blobStore) {
		return nil
	}
	// A blob store's driver, configuration reference and physical destination
	// are fixed at creation for every store, referenced or not: changing the
	// backend means creating another store and using the drain workflow, so a
	// definition edit can never silently repoint existing content. Only the
	// operational attributes remain editable below.
	if !blobStoreConfigurationEqual(existing, blobStore) {
		return domain.ErrBlobStoreDefinitionImmutable
	}
	configurationEnvironment, configurationFile := configurationReferenceColumns(blobStore)
	attributes, err := json.Marshal(blobStore.Attributes)
	if err != nil {
		return fmt.Errorf("encode blob store attributes: %w", err)
	}
	const updateQuery = `
        UPDATE blob_stores
		SET driver = ?, configuration_env = ?, configuration_file = ?,
			physical_identity = ?, attributes = ?
        WHERE name = ?`
	result, err := transaction.ExecContext(
		ctx,
		updateQuery,
		blobStore.Driver,
		configurationEnvironment,
		configurationFile,
		blobStore.PhysicalIdentity,
		string(attributes),
		blobStore.Name,
	)
	if err != nil {
		return mapBlobStoreMutationError(err)
	}
	return requireAffectedRow(result)
}

// SetBlobStoreState persists a blob store's lifecycle state and drain target.
// It is separate from UpdateBlobStore so an ordinary resource edit never
// touches lifecycle, and so the migrate job can advance state without rewriting
// the store definition. Transition validity is enforced by the caller.
func (s *SQLStore) SetBlobStoreState(
	ctx context.Context,
	name string,
	state string,
	drainTarget string,
) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if state == domain.BlobStoreStateDraining {
		if drainTarget == "" || drainTarget == name {
			return domain.ErrInvalidDrainTarget
		}
		// Lock both stores in a stable order: opposite drains cannot deadlock,
		// and target retirement uses this same target row lock.
		first, second := name, drainTarget
		if second < first {
			first, second = second, first
		}
		for _, locked := range []string{first, second} {
			if err := lockUploadSessionStore(ctx, transaction, locked); err != nil {
				if locked == drainTarget && errors.Is(err, domain.ErrNotFound) {
					return domain.ErrInvalidDrainTarget
				}
				return err
			}
		}
		var sourceState, targetState string
		if err := transaction.QueryRowContext(ctx,
			`SELECT state FROM blob_stores WHERE name = ?`, name,
		).Scan(&sourceState); err != nil {
			return err
		}
		if err := transaction.QueryRowContext(ctx,
			`SELECT state FROM blob_stores WHERE name = ?`, drainTarget,
		).Scan(&targetState); err != nil {
			return err
		}
		if sourceState != domain.BlobStoreStateActive {
			return domain.ErrBlobStoreNotDrainable
		}
		if targetState != domain.BlobStoreStateActive {
			return domain.ErrInvalidDrainTarget
		}
		// A destination must remain active until every source draining onto it
		// has finished or cancelled. Any competing drain onto this source
		// locks the same source row, so the check and transition are atomic.
		var incomingDrains int
		if err := transaction.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blob_stores WHERE state = 'draining' AND drain_target = ?`,
			name,
		).Scan(&incomingDrains); err != nil {
			return err
		}
		if incomingDrains > 0 {
			return domain.ErrActiveDrainTarget
		}
	}
	result, err := transaction.ExecContext(
		ctx,
		`UPDATE blob_stores SET state = ?, drain_target = ? WHERE name = ?`,
		state,
		drainTarget,
		name,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return err
	}
	return transaction.Commit()
}

// BeginBlobStoreDrain marks source draining onto target in one transaction.
// SetBlobStoreState locks both rows in name order and revalidates their states,
// including against concurrent target retirement and opposing drains.
func (s *SQLStore) BeginBlobStoreDrain(ctx context.Context, source string, target string) error {
	if source == "" || target == "" || source == target {
		return domain.ErrInvalidDrainTarget
	}
	if source == "default" {
		return domain.ErrBlobStoreNotDrainable
	}
	return s.SetBlobStoreState(ctx, source, domain.BlobStoreStateDraining, target)
}

// WriteBlobStore resolves where new bytes for a blob store must be written. A
// draining store takes no new blobs, so writes bound for it are redirected to
// its drain target (which the drain handler guarantees is an active store).
func (s *SQLStore) WriteBlobStore(ctx context.Context, blobStore string) (string, error) {
	if blobStore == "" {
		blobStore = "default"
	}
	resource, err := s.BlobStore(ctx, blobStore)
	if err != nil {
		return "", err
	}
	if resource.State == domain.BlobStoreStateDraining && resource.DrainTarget != "" {
		return resource.DrainTarget, nil
	}
	return blobStore, nil
}

// RebindRepositories moves every repository bound to one blob store onto
// another, together with the home store of every asset on that store, and
// returns how many repositories were rebound. It is the metadata half of a blob
// store migration and bypasses the API-level guard that blocks new bindings to
// a draining store: migration is exactly the sanctioned move onto its target.
// The asset move keeps per-asset resolution consistent with the repository
// binding once the migration has copied the bytes onto the target.
func (s *SQLStore) RebindRepositories(ctx context.Context, from string, to string) (int64, error) {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(
		ctx,
		`UPDATE repositories SET blob_store = ? WHERE blob_store = ?`,
		to,
		from,
	)
	if err != nil {
		return 0, err
	}
	rebound, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := transaction.ExecContext(
		ctx,
		`UPDATE assets SET blob_store = ? WHERE blob_store = ?`,
		to,
		from,
	); err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return rebound, nil
}

// DeleteBlobStore removes a blob store only when no active drain or physical
// reference still needs it.
func (s *SQLStore) DeleteBlobStore(ctx context.Context, name string, ownership Ownership) error {
	if name == "default" {
		return domain.ErrDefaultBlobStoreImmutable
	}
	return s.writeOwned(ctx, "blobStore", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			return deleteBlobStoreTx(ctx, transaction, name)
		})
}

// deleteBlobStoreTx removes a blob-store row within a caller's transaction,
// refusing a store that is a live drain target or still referenced by any
// repository, upload session, or asset.
func deleteBlobStoreTx(ctx context.Context, transaction *dialectTx, name string) error {
	// Upload creation takes the same row lock before inserting a session.
	// Holding it through the reference check and deletion prevents retirement
	// from racing an accepted upload on PostgreSQL as well as SQLite.
	if err := lockUploadSessionStore(ctx, transaction, name); err != nil {
		return err
	}
	// A drain and this deletion both lock the target row before checking state,
	// so whichever commits first determines whether deletion is safe.
	var activeDrains int
	if err := transaction.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blob_stores WHERE state = 'draining' AND drain_target = ?`,
		name,
	).Scan(&activeDrains); err != nil {
		return err
	}
	if activeDrains > 0 {
		return domain.ErrActiveDrainTarget
	}
	var references int
	if err := transaction.QueryRowContext(
		ctx,
		`SELECT (SELECT COUNT(*) FROM repositories WHERE blob_store = ?) +
		        (SELECT COUNT(*) FROM upload_sessions WHERE blob_store = ?) +
		        (SELECT COUNT(*) FROM assets WHERE blob_store = ?)`,
		name, name, name,
	).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return domain.ErrBlobStoreInUse
	}
	result, err := transaction.ExecContext(ctx, `DELETE FROM blob_stores WHERE name = ?`, name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "default blob store immutable") {
			return domain.ErrDefaultBlobStoreImmutable
		}
		if strings.Contains(strings.ToLower(err.Error()), "blob store in use") ||
			strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return domain.ErrBlobStoreInUse
		}
		return err
	}
	return requireAffectedRow(result)
}

// BlobStore returns one named blob storage backend.
func (s *SQLStore) BlobStore(ctx context.Context, name string) (domain.BlobStore, error) {
	query := `SELECT ` + blobStoreColumns + ` FROM blob_stores WHERE name = ?`
	return scanBlobStore(s.db.QueryRowContext(ctx, query, name))
}

// BlobStores returns all named blob storage backends ordered by name.
func (s *SQLStore) BlobStores(ctx context.Context) ([]domain.BlobStore, error) {
	query := `SELECT ` + blobStoreColumns + ` FROM blob_stores ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	blobStores := make([]domain.BlobStore, 0)
	for rows.Next() {
		blobStore, err := scanBlobStore(rows)
		if err != nil {
			return nil, err
		}
		blobStores = append(blobStores, blobStore)
	}
	return blobStores, rows.Err()
}

type blobStoreScanner interface {
	Scan(...any) error
}

func scanBlobStore(scanner blobStoreScanner) (domain.BlobStore, error) {
	var blobStore domain.BlobStore
	var configurationEnvironment string
	var configurationFile string
	var attributes string
	var createdAt string
	err := scanner.Scan(
		&blobStore.Name,
		&blobStore.Driver,
		&configurationEnvironment,
		&configurationFile,
		&blobStore.PhysicalIdentity,
		&attributes,
		&blobStore.State,
		&blobStore.DrainTarget,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return blobStore, domain.ErrNotFound
	}
	if err != nil {
		return blobStore, err
	}
	if configurationEnvironment != "" || configurationFile != "" {
		blobStore.ConfigurationRef = &domain.ConfigurationReference{
			Env:  configurationEnvironment,
			File: configurationFile,
		}
	}
	if err := decodeJSONNumbers(attributes, &blobStore.Attributes); err != nil {
		return blobStore, fmt.Errorf("decode blob store attributes: %w", err)
	}
	blobStore.CreatedAt, err = parseTime(createdAt)
	return blobStore, err
}

func blobStoreDefinitionEqual(left domain.BlobStore, right domain.BlobStore) bool {
	return blobStoreConfigurationEqual(left, right) &&
		reflect.DeepEqual(left.Attributes, right.Attributes)
}

func blobStoreConfigurationEqual(left domain.BlobStore, right domain.BlobStore) bool {
	if left.Driver != right.Driver || left.PhysicalIdentity != right.PhysicalIdentity {
		return false
	}
	leftEnvironment, leftFile := configurationReferenceColumns(left)
	rightEnvironment, rightFile := configurationReferenceColumns(right)
	return leftEnvironment == rightEnvironment && leftFile == rightFile
}

func mapBlobStoreMutationError(err error) error {
	message := strings.ToLower(err.Error())
	switch {
	case isBlobStoreIdentityConstraint(err):
		return domain.ErrBlobStoreIdentityConflict
	case strings.Contains(message, "blob store definition immutable"):
		return domain.ErrBlobStoreDefinitionImmutable
	case strings.Contains(message, "default blob store immutable"):
		return domain.ErrDefaultBlobStoreImmutable
	default:
		return err
	}
}

func isBlobStoreIdentityConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "blob_stores.physical_identity") ||
		strings.Contains(message, "idx_blob_stores_physical_identity")
}

func configurationReferenceColumns(blobStore domain.BlobStore) (string, string) {
	if blobStore.ConfigurationRef == nil {
		return "", ""
	}
	return blobStore.ConfigurationRef.Env, blobStore.ConfigurationRef.File
}

func normalizeBlobStore(blobStore *domain.BlobStore) {
	if blobStore.Attributes == nil {
		blobStore.Attributes = map[string]any{}
	}
	if blobStore.CreatedAt.IsZero() {
		blobStore.CreatedAt = time.Now().UTC()
	}
}
