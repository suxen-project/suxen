package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

const uploadSessionColumns = `
    id,
    storage_key,
    (SELECT name FROM repositories WHERE id = upload_sessions.repository_id),
    image,
    blob_store,
    principal,
    size_bytes,
    reserved_bytes,
    operation_id,
    operation_expires_at,
    created_at,
    updated_at`

// CreateUploadSession records a new transient upload after atomically enforcing
// session-count admission for its principal.
func (s *SQLStore) CreateUploadSession(
	ctx context.Context,
	session UploadSession,
	limits UploadSessionLimits,
) error {
	if err := validateUploadSession(session, limits); err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := lockUploadSessionStore(ctx, transaction, session.BlobStore); err != nil {
		return err
	}
	if session.RepositoryID == "" {
		return domain.ErrNotFound
	}
	// This row lock conflicts with repository deletion on PostgreSQL. SQLite's
	// immediate transaction serializes the two writes instead.
	repositoryQuery := `SELECT id FROM repositories WHERE id = ? AND name = ?`
	if s.dialect == dialectPostgres {
		repositoryQuery += ` FOR KEY SHARE`
	}
	var repositoryID string
	if err := transaction.QueryRowContext(ctx, repositoryQuery, session.RepositoryID, session.Repository).Scan(&repositoryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}

	var principalSessions int64
	if err := transaction.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM upload_sessions WHERE blob_store = ? AND principal = ?`,
		session.BlobStore,
		session.Principal,
	).Scan(&principalSessions); err != nil {
		return err
	}
	if principalSessions >= limits.MaxPrincipalSessions {
		return domain.ErrUploadSessionQuotaExceeded
	}

	const insert = `
        INSERT INTO upload_sessions (
            id, storage_key, repository_id, image, blob_store, principal,
            size_bytes, reserved_bytes, operation_id, operation_expires_at,
            created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', 0, ?, ?)`
	_, err = transaction.ExecContext(
		ctx,
		insert,
		session.ID,
		session.StorageKey,
		repositoryID,
		session.Image,
		session.BlobStore,
		session.Principal,
		formatTime(session.CreatedAt),
		formatTime(session.UpdatedAt),
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	return transaction.Commit()
}

// UploadSession returns one transient upload-session record.
func (s *SQLStore) UploadSession(ctx context.Context, id string) (UploadSession, error) {
	query := `SELECT ` + uploadSessionColumns + ` FROM upload_sessions WHERE id = ?`
	return scanUploadSession(s.db.QueryRowContext(ctx, query, id))
}

func (s *SQLStore) CountUploadSessions(ctx context.Context, blobStore string) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upload_sessions WHERE blob_store = ?`, blobStore,
	).Scan(&count)
	return count, err
}

// StaleUploadSessions returns one blob store's sessions whose last activity predates
// staleBefore and that hold no operation lease still live at now, oldest first.
func (s *SQLStore) StaleUploadSessions(
	ctx context.Context,
	blobStore string,
	staleBefore time.Time,
	now time.Time,
) ([]UploadSession, error) {
	// updated_at is a fixed-width UTC timestamp, so string ordering is time ordering.
	query := `SELECT ` + uploadSessionColumns + `
        FROM upload_sessions
        WHERE blob_store = ? AND updated_at < ?
          AND (operation_id = '' OR operation_expires_at <= ?)
        ORDER BY updated_at, id`
	rows, err := s.db.QueryContext(
		ctx,
		query,
		blobStore,
		formatTime(staleBefore),
		now.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []UploadSession
	for rows.Next() {
		session, err := scanUploadSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// ReserveUploadSession atomically leases one session operation and reserves aggregate
// capacity against both the physical store and the authenticated principal.
func (s *SQLStore) ReserveUploadSession(
	ctx context.Context,
	identity UploadSessionIdentity,
	operationID string,
	now time.Time,
	expiresAt time.Time,
	maxGrowth int64,
	exactGrowth bool,
	maxSessionBytes int64,
	limits UploadSessionLimits,
) (UploadSessionReservation, error) {
	return s.reserveUploadSession(ctx, identity, operationID, now, expiresAt,
		maxGrowth, exactGrowth, maxSessionBytes, limits, false)
}

// ReserveUploadSessionCleanup leases a session for deletion. A policy edit may
// lower current size or aggregate quotas below already staged bytes, but must
// not prevent cancellation or reclamation of those bytes.
func (s *SQLStore) ReserveUploadSessionCleanup(
	ctx context.Context,
	identity UploadSessionIdentity,
	operationID string,
	now time.Time,
	expiresAt time.Time,
) (UploadSessionReservation, error) {
	return s.reserveUploadSession(ctx, identity, operationID, now, expiresAt,
		0, true, 0, UploadSessionLimits{}, true)
}

func (s *SQLStore) reserveUploadSession(
	ctx context.Context,
	identity UploadSessionIdentity,
	operationID string,
	now time.Time,
	expiresAt time.Time,
	maxGrowth int64,
	exactGrowth bool,
	maxSessionBytes int64,
	limits UploadSessionLimits,
	cleanup bool,
) (UploadSessionReservation, error) {
	if operationID == "" || expiresAt.Compare(now) <= 0 ||
		!cleanup && (maxGrowth < 0 || maxSessionBytes <= 0 || validateUploadSessionLimits(limits) != nil) {
		return UploadSessionReservation{}, errors.New("invalid upload-session reservation")
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionReservation{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := lockUploadSessionStore(ctx, transaction, identity.BlobStore); err != nil {
		return UploadSessionReservation{}, err
	}
	if _, err := transaction.ExecContext(
		ctx,
		`UPDATE upload_sessions
		 SET operation_id = '', operation_expires_at = 0
		 WHERE blob_store = ? AND operation_id <> '' AND operation_expires_at <= ?`,
		identity.BlobStore,
		now.UnixNano(),
	); err != nil {
		return UploadSessionReservation{}, err
	}

	query := `SELECT ` + uploadSessionColumns + ` FROM upload_sessions WHERE id = ?`
	if s.dialect == dialectPostgres {
		query += ` FOR UPDATE`
	}
	session, err := scanUploadSession(transaction.QueryRowContext(ctx, query, identity.ID))
	if err != nil {
		return UploadSessionReservation{}, err
	}
	if !uploadSessionIdentityEqual(session.UploadSessionIdentity, identity) {
		return UploadSessionReservation{}, domain.ErrNotFound
	}
	if session.OperationID != "" {
		return UploadSessionReservation{}, domain.ErrUploadSessionBusy
	}
	// An expired or interrupted append can leave physical bytes that have not
	// reached size_bytes yet. Keep its conservative reservation charged until
	// the caller reconciles the staged object under a cleanup-style lease.
	if !cleanup && session.ReservedBytes != 0 {
		return UploadSessionReservation{}, domain.ErrUploadSessionBusy
	}
	reservedGrowth := int64(0)
	if !cleanup {
		if session.Size > maxSessionBytes || exactGrowth && maxGrowth > maxSessionBytes-session.Size {
			return UploadSessionReservation{}, domain.ErrUploadSessionSizeExceeded
		}
		if !exactGrowth && maxGrowth > maxSessionBytes-session.Size {
			maxGrowth = maxSessionBytes - session.Size
		}
		storeUsed, err := uploadSessionBytes(
			ctx,
			transaction,
			`SELECT COALESCE(SUM(size_bytes + reserved_bytes), 0)
         FROM upload_sessions WHERE blob_store = ?`,
			identity.BlobStore,
		)
		if err != nil {
			return UploadSessionReservation{}, err
		}
		principalUsed, err := uploadSessionBytes(
			ctx,
			transaction,
			`SELECT COALESCE(SUM(size_bytes + reserved_bytes), 0)
         FROM upload_sessions WHERE blob_store = ? AND principal = ?`,
			identity.BlobStore,
			identity.Principal,
		)
		if err != nil {
			return UploadSessionReservation{}, err
		}
		available := minInt64(
			limits.MaxStagedBytes-storeUsed,
			limits.MaxPrincipalStagedBytes-principalUsed,
		)
		reservedGrowth = maxGrowth
		if !exactGrowth && reservedGrowth > available {
			reservedGrowth = available
		}
		if reservedGrowth > available || maxGrowth > 0 && reservedGrowth <= 0 {
			return UploadSessionReservation{}, domain.ErrUploadSessionQuotaExceeded
		}
	}

	reservationBytes := reservedGrowth
	reservationUpdatedAt := session.UpdatedAt
	if cleanup {
		reservationBytes = session.ReservedBytes
	} else {
		// A successful reservation is upload activity even if the process dies
		// after writing bytes and before committing their final size. Record it
		// now so stale-session reaping cannot discard a newly resumed upload.
		reservationUpdatedAt = now
	}
	const reserve = `
        UPDATE upload_sessions
		SET reserved_bytes = ?, operation_id = ?, operation_expires_at = ?, updated_at = ?
        WHERE id = ? AND operation_id = ''`
	result, err := transaction.ExecContext(
		ctx,
		reserve,
		reservationBytes,
		operationID,
		expiresAt.UnixNano(),
		formatTime(reservationUpdatedAt),
		identity.ID,
	)
	if err != nil {
		return UploadSessionReservation{}, err
	}
	if err := requireAffectedRow(result); err != nil {
		return UploadSessionReservation{}, domain.ErrUploadSessionBusy
	}
	if err := transaction.Commit(); err != nil {
		return UploadSessionReservation{}, err
	}
	session.ReservedBytes = reservationBytes
	session.OperationID = operationID
	session.OperationExpires = expiresAt.UTC()
	session.UpdatedAt = reservationUpdatedAt
	return UploadSessionReservation{
		Session:      session,
		ReservedGrow: reservedGrowth,
		MaxSize:      session.Size + reservedGrowth,
	}, nil
}

// CommitUploadSessionAppend records the durable size produced by one leased append.
func (s *SQLStore) CommitUploadSessionAppend(
	ctx context.Context,
	id string,
	operationID string,
	size int64,
	updatedAt time.Time,
	retainOperation bool,
) error {
	if operationID == "" || size < 0 {
		return errors.New("invalid upload-session append commit")
	}
	operationValue := ""
	var expiryValue int64
	if retainOperation {
		operationValue = operationID
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT operation_expires_at FROM upload_sessions WHERE id = ? AND operation_id = ?`,
			id,
			operationID,
		).Scan(&expiryValue); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE upload_sessions
         SET size_bytes = ?, reserved_bytes = 0, operation_id = ?,
             operation_expires_at = ?, updated_at = ?
         WHERE id = ? AND operation_id = ? AND size_bytes + reserved_bytes >= ?`,
		size,
		operationValue,
		expiryValue,
		formatTime(updatedAt),
		id,
		operationID,
		size,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return nil
}

// ReconcileUploadSessionSize accounts for bytes already present in the staged
// object after an interrupted append. It requires the caller's cleanup-style
// operation lease, so only the physical size observed while holding that lease
// can become authoritative. No growth quota is checked here: already-staged
// bytes must be counted even if an operator has since lowered the limits.
func (s *SQLStore) ReconcileUploadSessionSize(
	ctx context.Context,
	identity UploadSessionIdentity,
	operationID string,
	size int64,
	updatedAt time.Time,
) error {
	if operationID == "" || size < 0 {
		return errors.New("invalid upload-session size reconciliation")
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	// Quota reservations and reconciliation use the same store row lock. The
	// reconciled bytes are therefore visible to the next capacity decision.
	if err := lockUploadSessionStore(ctx, transaction, identity.BlobStore); err != nil {
		return err
	}
	result, err := transaction.ExecContext(ctx,
		`UPDATE upload_sessions
		 SET size_bytes = ?, reserved_bytes = 0, operation_id = '',
		     operation_expires_at = 0, updated_at = ?
		 WHERE id = ? AND operation_id = ?
		   AND repository_id = (SELECT id FROM repositories WHERE name = ?)
		   AND image = ? AND blob_store = ? AND principal = ?`,
		size, formatTime(updatedAt), identity.ID, operationID,
		identity.Repository, identity.Image, identity.BlobStore, identity.Principal,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return transaction.Commit()
}

// RenewUploadSessionOperation extends a still-live operation lease without
// refreshing upload activity or altering reserved capacity. A crashed process
// stops renewing, so the existing expiry-based recovery remains effective.
func (s *SQLStore) RenewUploadSessionOperation(
	ctx context.Context,
	id string,
	operationID string,
	now time.Time,
	expiresAt time.Time,
) error {
	if operationID == "" || !expiresAt.After(now) {
		return errors.New("invalid upload-session lease renewal")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE upload_sessions SET operation_expires_at = ?
		 WHERE id = ? AND operation_id = ? AND operation_expires_at > ?`,
		expiresAt.UnixNano(), id, operationID, now.UnixNano(),
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return nil
}

// ReleaseUploadSessionOperation releases a lease without refreshing session activity.
func (s *SQLStore) ReleaseUploadSessionOperation(
	ctx context.Context,
	id string,
	operationID string,
) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE upload_sessions
		 SET reserved_bytes = 0, operation_id = '', operation_expires_at = 0
         WHERE id = ? AND operation_id = ?`,
		id,
		operationID,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return nil
}

// ReleaseUploadSessionOperationUncertain drops an operation lease without
// releasing its reserved capacity. A physical append may have succeeded even
// when its ledger commit failed, so the hold remains charged until the next
// reconciliation or stale-session deletion.
func (s *SQLStore) ReleaseUploadSessionOperationUncertain(
	ctx context.Context,
	id string,
	operationID string,
) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE upload_sessions
		 SET operation_id = '', operation_expires_at = 0
		 WHERE id = ? AND operation_id = ?`,
		id, operationID,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteUploadSession removes a completed session or compensates a failed creation.
func (s *SQLStore) DeleteUploadSession(
	ctx context.Context,
	id string,
	operationID string,
) error {
	query := `DELETE FROM upload_sessions WHERE id = ?`
	arguments := []any{id}
	if operationID != "" {
		query += ` AND operation_id = ?`
		arguments = append(arguments, operationID)
	} else {
		query += ` AND operation_id = ''`
	}
	result, err := s.db.ExecContext(ctx, query, arguments...)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return domain.ErrNotFound
	}
	return nil
}

func lockUploadSessionStore(
	ctx context.Context,
	transaction *dialectTx,
	blobStore string,
) error {
	result, err := transaction.ExecContext(
		ctx,
		`UPDATE blob_stores SET name = name WHERE name = ?`,
		blobStore,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

func uploadSessionBytes(
	ctx context.Context,
	transaction *dialectTx,
	query string,
	arguments ...any,
) (int64, error) {
	var bytes int64
	err := transaction.QueryRowContext(ctx, query, arguments...).Scan(&bytes)
	return bytes, err
}

func validateUploadSession(session UploadSession, limits UploadSessionLimits) error {
	if session.ID == "" || session.StorageKey == "" || session.Repository == "" ||
		session.Image == "" || session.BlobStore == "" || session.Principal == "" ||
		session.Size != 0 || session.ReservedBytes != 0 ||
		session.OperationID != "" || session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() {
		return errors.New("invalid upload session")
	}
	return validateUploadSessionLimits(limits)
}

func validateUploadSessionLimits(limits UploadSessionLimits) error {
	if limits.MaxStagedBytes <= 0 || limits.MaxPrincipalStagedBytes <= 0 ||
		limits.MaxPrincipalSessions <= 0 {
		return errors.New("invalid upload-session limits")
	}
	return nil
}

func uploadSessionIdentityEqual(left UploadSessionIdentity, right UploadSessionIdentity) bool {
	return left == right
}

func minInt64(left int64, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

type uploadSessionScanner interface {
	Scan(...any) error
}

func scanUploadSession(scanner uploadSessionScanner) (UploadSession, error) {
	var session UploadSession
	var operationExpires int64
	var createdAt string
	var updatedAt string
	err := scanner.Scan(
		&session.ID,
		&session.StorageKey,
		&session.Repository,
		&session.Image,
		&session.BlobStore,
		&session.Principal,
		&session.Size,
		&session.ReservedBytes,
		&session.OperationID,
		&operationExpires,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadSession{}, domain.ErrNotFound
	}
	if err != nil {
		return UploadSession{}, err
	}
	if operationExpires != 0 {
		session.OperationExpires = time.Unix(0, operationExpires).UTC()
	}
	session.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return UploadSession{}, fmt.Errorf("parse upload creation time: %w", err)
	}
	session.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return UploadSession{}, fmt.Errorf("parse upload update time: %w", err)
	}
	return session, nil
}
