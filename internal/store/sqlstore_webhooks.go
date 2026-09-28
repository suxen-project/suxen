package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
)

const webhookColumns = `
	name,
	url,
	secret,
	events,
	repositories,
	enabled,
	created_at,
	updated_at`

const webhookDeliveryColumns = `
	d.id,
	d.webhook_name,
	d.event,
	d.repository,
	d.payload,
	d.status,
	d.attempts,
	d.next_attempt_at,
	d.last_error,
	d.created_at,
	d.updated_at,
	d.delivered_at,
	w.url,
	w.secret`

// CreateWebhook inserts a name-keyed outbound event subscription.
func (s *SQLStore) CreateWebhook(ctx context.Context, webhook domain.Webhook) error {
	if err := s.validateWebhook(ctx, webhook); err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
		return err
	}
	if err := validateRepositoryFilterTx(ctx, transaction, webhook.Repositories); err != nil {
		return err
	}
	if err := insertWebhookRow(ctx, transaction, webhook); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	return s.invalidateProvisionSecret(ctx, "webhook", webhook.Name)
}

// WebhookSave is an atomic webhook mutation and its ownership effect. Create
// inserts a new subscription; an empty Secret keeps the existing one on update.
type WebhookSave struct {
	Webhook   domain.Webhook
	Create    bool
	Ownership Ownership
}

// SaveWebhook commits a webhook subscription and its ownership record in one
// transaction serialized on ("webhook", name).
func (s *SQLStore) SaveWebhook(ctx context.Context, save WebhookSave) error {
	webhook := save.Webhook
	validation := webhook
	if !save.Create && validation.Secret == "" {
		validation.Secret = webhookSecretValidationPlaceholder
	}
	if err := s.validateWebhook(ctx, validation); err != nil {
		return err
	}
	return s.writeOwned(ctx, "webhook", webhook.Name, save.Ownership, !save.Create, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
				return err
			}
			if err := validateRepositoryFilterTx(ctx, transaction, webhook.Repositories); err != nil {
				return err
			}
			if save.Create {
				return insertWebhookRow(ctx, transaction, webhook)
			}
			_, _, err := upsertWebhookRow(ctx, transaction, webhook)
			return err
		})
}

// DeleteWebhook removes a subscription (and its delivery history) with its
// ownership record in one transaction serialized on ("webhook", name).
func (s *SQLStore) DeleteWebhook(ctx context.Context, name string, ownership Ownership) error {
	return s.writeOwned(ctx, "webhook", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(ctx, `DELETE FROM webhooks WHERE name = ?`, name)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

const webhookSecretValidationPlaceholder = "omitted-secret-validation-placeholder"

func insertWebhookRow(ctx context.Context, executor sqlExecer, webhook domain.Webhook) error {
	events, repositories, err := encodeWebhookFilters(webhook)
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	const query = `
		INSERT INTO webhooks (
			name, url, secret, events, repositories, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = executor.ExecContext(
		ctx,
		query,
		webhook.Name,
		webhook.URL,
		webhook.Secret,
		events,
		repositories,
		webhook.Enabled,
		now,
		now,
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

// Webhook returns one outbound subscription by name.
func (s *SQLStore) Webhook(
	ctx context.Context,
	name string,
) (domain.Webhook, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhooks WHERE name = ?`
	return scanWebhook(s.db.QueryRowContext(ctx, query, name))
}

// PutWebhook creates or replaces a named outbound subscription.
func (s *SQLStore) PutWebhook(
	ctx context.Context,
	webhook domain.Webhook,
) (domain.Webhook, error) {
	validationWebhook := webhook
	if validationWebhook.Secret == "" {
		validationWebhook.Secret = webhookSecretValidationPlaceholder
	}
	if err := s.validateWebhook(ctx, validationWebhook); err != nil {
		return domain.Webhook{}, err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Webhook{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := s.lockRepositoryRelations(ctx, transaction); err != nil {
		return domain.Webhook{}, err
	}
	if err := validateRepositoryFilterTx(ctx, transaction, webhook.Repositories); err != nil {
		return domain.Webhook{}, err
	}
	stored, secretChanged, err := upsertWebhookRow(ctx, transaction, webhook)
	if err != nil {
		return domain.Webhook{}, err
	}
	if err := transaction.Commit(); err != nil {
		return domain.Webhook{}, err
	}
	if secretChanged {
		if err := s.invalidateProvisionSecret(ctx, "webhook", webhook.Name); err != nil {
			return domain.Webhook{}, err
		}
	}
	return stored, nil
}

// upsertWebhookRow creates or replaces a subscription. An empty secret keeps the
// stored one (and rejects a create). It reports whether the secret changed so a
// standalone caller can invalidate the ownership fingerprint outside a
// transaction.
func upsertWebhookRow(
	ctx context.Context,
	executor sqlExecer,
	webhook domain.Webhook,
) (domain.Webhook, bool, error) {
	events, repositories, err := encodeWebhookFilters(webhook)
	if err != nil {
		return domain.Webhook{}, false, err
	}
	now := formatTime(time.Now().UTC())
	if webhook.Secret == "" {
		const update = `
			UPDATE webhooks
			SET url = ?, events = ?, repositories = ?, enabled = ?, updated_at = ?
			WHERE name = ?
			RETURNING ` + webhookColumns
		stored, err := scanWebhook(executor.QueryRowContext(
			ctx,
			update,
			webhook.URL,
			events,
			repositories,
			webhook.Enabled,
			now,
			webhook.Name,
		))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Webhook{}, false, domain.ErrWebhookSecretRequired
		}
		return stored, false, err
	}
	secretChanged := true
	var existingSecret string
	err = executor.QueryRowContext(
		ctx,
		`SELECT secret FROM webhooks WHERE name = ?`,
		webhook.Name,
	).Scan(&existingSecret)
	if err == nil {
		secretChanged = !secretValuesEqual(existingSecret, webhook.Secret)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.Webhook{}, false, err
	}

	const query = `
		INSERT INTO webhooks (
			name, url, secret, events, repositories, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			url = excluded.url,
			secret = excluded.secret,
			events = excluded.events,
			repositories = excluded.repositories,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
		RETURNING ` + webhookColumns
	stored, err := scanWebhook(executor.QueryRowContext(
		ctx,
		query,
		webhook.Name,
		webhook.URL,
		webhook.Secret,
		events,
		repositories,
		webhook.Enabled,
		now,
		now,
	))
	if err != nil {
		return domain.Webhook{}, false, err
	}
	return stored, secretChanged, nil
}

// Webhooks returns all outbound subscriptions ordered by name.
func (s *SQLStore) Webhooks(ctx context.Context) ([]domain.Webhook, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhooks ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	webhooks := make([]domain.Webhook, 0)
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, webhook)
	}
	return webhooks, rows.Err()
}

// EnqueueWebhookEvent creates deliveries for matching enabled subscriptions.
func (s *SQLStore) EnqueueWebhookEvent(
	ctx context.Context,
	event domain.WebhookEvent,
) error {
	if !domain.IsWebhookEventType(event.Type) {
		return domain.ErrInvalidWebhookEvent
	}
	if event.Repository == "" {
		return domain.ErrInvalidRepository
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()

	if err := enqueueWebhookEventTx(ctx, transaction, event); err != nil {
		return err
	}
	return transaction.Commit()
}

func (s *SQLStore) enqueueAssetUploadedTx(
	ctx context.Context,
	transaction *dialectTx,
	asset domain.Asset,
	repository domain.Repository,
	occurredAt time.Time,
) error {
	asset.Attributes = assetattrs.Project(asset, repository)
	events := []string{domain.WebhookAssetUploaded}
	if asset.CreatedAt.Equal(asset.UpdatedAt) &&
		(asset.Kind == "raw" || (asset.Kind == "oci-manifest" && asset.Reference != "" && !strings.HasPrefix(asset.Reference, "sha256:"))) {
		events = append(events, domain.WebhookComponentCreated)
	}
	for _, eventType := range events {
		event := domain.WebhookEvent{
			ID: randomWebhookEventID(), Type: eventType, Repository: asset.Repository,
			OccurredAt: occurredAt, Asset: &asset,
		}
		if err := enqueueWebhookEventTx(ctx, transaction, event); err != nil {
			return err
		}
	}
	return nil
}

func enqueueWebhookEventTx(
	ctx context.Context,
	transaction *dialectTx,
	event domain.WebhookEvent,
) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode webhook event: %w", err)
	}
	query := `SELECT ` + webhookColumns + ` FROM webhooks ORDER BY name`
	if transaction.dialect == dialectPostgres {
		// Delivery rows refer to subscriptions by name. Hold selected rows
		// through insertion so deletion and recreation under the same name
		// cannot attach this event to a different subscription.
		query += ` FOR SHARE`
	}
	rows, err := transaction.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	var webhooks []domain.Webhook
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return err
		}
		webhooks = append(webhooks, webhook)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	const insert = `
		INSERT INTO webhook_deliveries (
			webhook_name, event, repository, payload, status, attempts,
			next_attempt_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'queued', 0, ?, ?, ?)`
	for _, webhook := range webhooks {
		if !webhook.Enabled || !webhookMatchesEvent(webhook, event) {
			continue
		}
		formatted := formatTime(event.OccurredAt)
		if _, err := transaction.ExecContext(
			ctx, insert, webhook.Name, event.Type, event.Repository,
			string(payload), formatted, formatted, formatted,
		); err != nil {
			return err
		}
	}
	return nil
}

func randomWebhookEventID() string {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		// crypto/rand failure will also prevent secure server operation elsewhere;
		// retain uniqueness within this process for the error path.
		return fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

// ClaimWebhookDeliveries leases due work to one cluster worker.
func (s *SQLStore) ClaimWebhookDeliveries(
	ctx context.Context,
	worker string,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) ([]domain.WebhookDelivery, error) {
	if worker == "" {
		return nil, errors.New("webhook worker name is required")
	}
	if limit <= 0 || limit > 100 {
		return nil, errors.New("webhook delivery claim limit must be between 1 and 100")
	}

	const candidatesQuery = `
		SELECT d.id
		FROM webhook_deliveries d
		JOIN webhooks w ON w.name = d.webhook_name
		WHERE w.enabled = ?
			AND d.next_attempt_at <= ?
			AND (
				d.status IN ('queued', 'retry')
				OR (
					d.status = 'delivering'
					AND (d.locked_until IS NULL OR d.locked_until <= ?)
				)
			)
		ORDER BY d.next_attempt_at, d.id
		LIMIT ?`
	rows, err := s.db.QueryContext(
		ctx,
		candidatesQuery,
		true,
		formatTime(now),
		formatTime(now),
		limit,
	)
	if err != nil {
		return nil, err
	}
	candidateIDs := make([]int64, 0, limit)
	for rows.Next() {
		var deliveryID int64
		if err := rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return nil, err
		}
		candidateIDs = append(candidateIDs, deliveryID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	claimed := make([]domain.WebhookDelivery, 0, len(candidateIDs))
	for _, deliveryID := range candidateIDs {
		delivery, won, err := s.claimWebhookDelivery(
			ctx,
			deliveryID,
			worker,
			now,
			now.Add(leaseDuration),
		)
		if err != nil {
			return nil, err
		}
		if won {
			claimed = append(claimed, delivery)
		}
	}
	return claimed, nil
}

func (s *SQLStore) claimWebhookDelivery(
	ctx context.Context,
	deliveryID int64,
	worker string,
	now time.Time,
	lockedUntil time.Time,
) (domain.WebhookDelivery, bool, error) {
	const claim = `
		UPDATE webhook_deliveries
		SET status = 'delivering', locked_by = ?, locked_until = ?,
			attempts = attempts + 1, updated_at = ?
		WHERE id = ?
			AND next_attempt_at <= ?
			AND (
				status IN ('queued', 'retry')
				OR (
					status = 'delivering'
					AND (locked_until IS NULL OR locked_until <= ?)
				)
			)`
	result, err := s.db.ExecContext(
		ctx,
		claim,
		worker,
		formatTime(lockedUntil),
		formatTime(now),
		deliveryID,
		formatTime(now),
		formatTime(now),
	)
	if err != nil {
		return domain.WebhookDelivery{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return domain.WebhookDelivery{}, false, err
	}

	query := `
		SELECT ` + webhookDeliveryColumns + `
		FROM webhook_deliveries d
		JOIN webhooks w ON w.name = d.webhook_name
		WHERE d.id = ? AND d.locked_by = ?`
	delivery, err := scanWebhookDelivery(
		s.db.QueryRowContext(ctx, query, deliveryID, worker),
	)
	return delivery, err == nil, err
}

// CompleteWebhookDelivery records a delivery's success, retry, or dead-letter state.
func (s *SQLStore) CompleteWebhookDelivery(
	ctx context.Context,
	deliveryID int64,
	worker string,
	status string,
	nextAttemptAt time.Time,
	lastError string,
) error {
	if status != "delivered" && status != "retry" && status != "dead" {
		return errors.New("invalid webhook delivery completion status")
	}
	now := time.Now().UTC()
	deliveredAt := sql.NullString{}
	if status == "delivered" {
		deliveredAt = sql.NullString{String: formatTime(now), Valid: true}
	}
	const query = `
		UPDATE webhook_deliveries
		SET status = ?, next_attempt_at = ?, last_error = ?,
			locked_by = NULL, locked_until = NULL, updated_at = ?, delivered_at = ?
		WHERE id = ? AND status = 'delivering' AND locked_by = ?`
	result, err := s.db.ExecContext(
		ctx,
		query,
		status,
		formatTime(nextAttemptAt),
		lastError,
		formatTime(now),
		deliveredAt,
		deliveryID,
		worker,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

// WebhookDeliveries returns recent history for one subscription name.
func (s *SQLStore) WebhookDeliveries(
	ctx context.Context,
	webhookName string,
	limit int,
) ([]domain.WebhookDelivery, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("webhook delivery limit must be between 1 and 1000")
	}
	if _, err := s.Webhook(ctx, webhookName); err != nil {
		return nil, err
	}
	query := `
		SELECT ` + webhookDeliveryColumns + `
		FROM webhook_deliveries d
		JOIN webhooks w ON w.name = d.webhook_name
		WHERE d.webhook_name = ?
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, webhookName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	deliveries := make([]domain.WebhookDelivery, 0)
	for rows.Next() {
		delivery, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

// WebhookDeliveryPage returns a bounded page ordered by descending delivery
// ID. A zero snapshot starts a traversal; continuations reuse SnapshotID and
// pass the last item ID as beforeID.
func (s *SQLStore) WebhookDeliveryPage(
	ctx context.Context,
	webhookName string,
	snapshotID int64,
	beforeID int64,
	limit int,
) (IDPage[domain.WebhookDelivery], error) {
	page := IDPage[domain.WebhookDelivery]{}
	if snapshotID < 0 || beforeID < 0 || limit < 1 || limit > 1000 {
		return page, errors.New("invalid webhook delivery page")
	}
	if _, err := s.Webhook(ctx, webhookName); err != nil {
		return page, err
	}
	if snapshotID == 0 {
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT COALESCE(MAX(id), 0) FROM webhook_deliveries WHERE webhook_name = ?`,
			webhookName,
		).Scan(&snapshotID); err != nil {
			return page, err
		}
	}
	page.SnapshotID = snapshotID
	query := `
		SELECT ` + webhookDeliveryColumns + `
		FROM webhook_deliveries d
		JOIN webhooks w ON w.name = d.webhook_name
		WHERE d.webhook_name = ? AND d.id <= ?`
	arguments := []any{webhookName, snapshotID}
	if beforeID > 0 {
		query += ` AND d.id < ?`
		arguments = append(arguments, beforeID)
	}
	query += ` ORDER BY d.id DESC LIMIT ?`
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	items := make([]domain.WebhookDelivery, 0, limit+1)
	for rows.Next() {
		delivery, err := scanWebhookDelivery(rows)
		if err != nil {
			return page, err
		}
		items = append(items, delivery)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(items) > limit {
		page.HasMore = true
		items = items[:limit]
	}
	page.Items = items
	return page, nil
}

func (s *SQLStore) validateWebhook(
	ctx context.Context,
	webhook domain.Webhook,
) error {
	if err := webhook.Validate(); err != nil {
		return err
	}
	for _, repositoryName := range webhook.Repositories {
		if _, err := s.Repository(ctx, repositoryName); err != nil {
			return fmt.Errorf("resolve webhook repository %q: %w", repositoryName, err)
		}
	}
	return nil
}

func encodeWebhookFilters(webhook domain.Webhook) (string, string, error) {
	events, err := json.Marshal(webhook.Events)
	if err != nil {
		return "", "", fmt.Errorf("encode webhook events: %w", err)
	}
	repositories, err := json.Marshal(webhook.Repositories)
	if err != nil {
		return "", "", fmt.Errorf("encode webhook repositories: %w", err)
	}
	return string(events), string(repositories), nil
}

func scanWebhook(source scanner) (domain.Webhook, error) {
	var webhook domain.Webhook
	var events string
	var repositories string
	var createdAt string
	var updatedAt string
	err := source.Scan(
		&webhook.Name,
		&webhook.URL,
		&webhook.Secret,
		&events,
		&repositories,
		&webhook.Enabled,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return webhook, domain.ErrNotFound
	}
	if err != nil {
		return webhook, err
	}
	if err := json.Unmarshal([]byte(events), &webhook.Events); err != nil {
		return webhook, fmt.Errorf("decode webhook events: %w", err)
	}
	if err := json.Unmarshal([]byte(repositories), &webhook.Repositories); err != nil {
		return webhook, fmt.Errorf("decode webhook repositories: %w", err)
	}
	webhook.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return webhook, err
	}
	webhook.UpdatedAt, err = parseTime(updatedAt)
	return webhook, err
}

func scanWebhookDelivery(source scanner) (domain.WebhookDelivery, error) {
	var delivery domain.WebhookDelivery
	var payload string
	var nextAttemptAt string
	var createdAt string
	var updatedAt string
	var deliveredAt sql.NullString
	err := source.Scan(
		&delivery.ID,
		&delivery.WebhookName,
		&delivery.Event,
		&delivery.Repository,
		&payload,
		&delivery.Status,
		&delivery.Attempts,
		&nextAttemptAt,
		&delivery.LastError,
		&createdAt,
		&updatedAt,
		&deliveredAt,
		&delivery.TargetURL,
		&delivery.Secret,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery, domain.ErrNotFound
	}
	if err != nil {
		return delivery, err
	}
	delivery.Payload = json.RawMessage(payload)
	delivery.NextAttemptAt, err = parseTime(nextAttemptAt)
	if err != nil {
		return delivery, err
	}
	delivery.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return delivery, err
	}
	delivery.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return delivery, err
	}
	if deliveredAt.Valid {
		parsed, err := parseTime(deliveredAt.String)
		if err != nil {
			return delivery, err
		}
		delivery.DeliveredAt = &parsed
	}
	return delivery, nil
}

func webhookMatchesEvent(
	webhook domain.Webhook,
	event domain.WebhookEvent,
) bool {
	eventMatched := false
	for _, subscribed := range webhook.Events {
		if subscribed == event.Type {
			eventMatched = true
			break
		}
	}
	if !eventMatched {
		return false
	}
	if len(webhook.Repositories) == 0 {
		return true
	}
	for _, repository := range webhook.Repositories {
		if repository == event.Repository {
			return true
		}
	}
	return false
}
