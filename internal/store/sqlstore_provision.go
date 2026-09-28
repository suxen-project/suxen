package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// ProvisionRecord returns reconciliation metadata for one managed resource.
func (s *SQLStore) ProvisionRecord(
	ctx context.Context,
	kind string,
	name string,
) (ProvisionRecord, error) {
	const query = `
		SELECT kind, name, secret_fingerprint, updated_at
		FROM provision_records
		WHERE kind = ? AND name = ?`
	return scanProvisionRecord(s.db.QueryRowContext(ctx, query, kind, name))
}

// ProvisionRecords returns all reconciliation metadata ordered by resource key.
func (s *SQLStore) ProvisionRecords(ctx context.Context) ([]ProvisionRecord, error) {
	const query = `
		SELECT kind, name, secret_fingerprint, updated_at
		FROM provision_records
		ORDER BY kind, name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]ProvisionRecord, 0)
	for rows.Next() {
		record, err := scanProvisionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// PutProvisionRecord creates or replaces reconciliation metadata.
func (s *SQLStore) PutProvisionRecord(ctx context.Context, record ProvisionRecord) error {
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO provision_records (
			kind, name, secret_fingerprint, updated_at
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, name) DO UPDATE SET
			secret_fingerprint = excluded.secret_fingerprint,
			updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(
		ctx,
		query,
		record.Kind,
		record.Name,
		record.SecretFingerprint,
		formatTime(record.UpdatedAt),
	)
	return err
}

// DeleteProvisionRecord forgets declarative ownership without deleting the resource.
func (s *SQLStore) DeleteProvisionRecord(ctx context.Context, kind string, name string) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM provision_records WHERE kind = ? AND name = ?`,
		kind,
		name,
	)
	return err
}

func (s *SQLStore) invalidateProvisionSecret(
	ctx context.Context,
	kind string,
	name string,
) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE provision_records
		 SET secret_fingerprint = '', updated_at = ?
		 WHERE kind = ? AND name = ?`,
		formatTime(time.Now().UTC()),
		kind,
		name,
	)
	return err
}

type provisionRecordScanner interface {
	Scan(...any) error
}

func scanProvisionRecord(scanner provisionRecordScanner) (ProvisionRecord, error) {
	var record ProvisionRecord
	var updatedAt string
	err := scanner.Scan(
		&record.Kind,
		&record.Name,
		&record.SecretFingerprint,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return record, domain.ErrNotFound
	}
	if err != nil {
		return record, err
	}
	record.UpdatedAt, err = parseTime(updatedAt)
	return record, err
}
