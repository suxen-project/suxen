package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestSQLitePutWebhookIsIdempotent(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "raw",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}

	created, err := metadata.PutWebhook(ctx, domain.Webhook{
		Name:         "scanner",
		URL:          "https://scanner.example/hooks/initial",
		Secret:       "named-webhook-secret",
		Events:       []string{domain.WebhookAssetUploaded},
		Repositories: []string{"raw"},
		Enabled:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	byName, err := metadata.Webhook(ctx, "scanner")
	if err != nil {
		t.Fatal(err)
	}
	if byName.Name != created.Name || byName.Secret != "named-webhook-secret" {
		t.Fatalf("named webhook lookup returned %+v, want scanner with its secret", byName)
	}

	updated, err := metadata.PutWebhook(ctx, domain.Webhook{
		Name:         "scanner",
		URL:          "https://scanner.example/hooks/updated",
		Events:       []string{domain.WebhookAssetDeleted},
		Repositories: []string{"raw"},
		Enabled:      false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != created.Name || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("named webhook identity changed: created=%+v updated=%+v", created, updated)
	}
	if updated.Secret != "named-webhook-secret" {
		t.Fatal("named webhook upsert replaced an omitted secret")
	}
	if updated.URL != "https://scanner.example/hooks/updated" || updated.Enabled {
		t.Fatalf("named webhook fields were not replaced: %+v", updated)
	}
	webhooks, err := metadata.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(webhooks) != 1 {
		t.Fatalf("idempotent named upsert created %d webhooks, want 1", len(webhooks))
	}

	_, err = metadata.PutWebhook(ctx, domain.Webhook{
		Name:    "missing-secret",
		URL:     "https://scanner.example/hooks/missing",
		Events:  []string{domain.WebhookAssetUploaded},
		Enabled: true,
	})
	if !errors.Is(err, domain.ErrWebhookSecretRequired) {
		t.Fatalf("secretless named create returned %v, want ErrWebhookSecretRequired", err)
	}
}

func TestSQLiteWebhookSchemaUsesNameAsIdentity(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	rows, err := metadata.db.QueryContext(ctx, `PRAGMA table_info(webhooks)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	primaryKeyColumn := ""
	for rows.Next() {
		var columnID int
		var name string
		var dataType string
		var notNull int
		var defaultValue any
		var primaryKeyPosition int
		if err := rows.Scan(
			&columnID,
			&name,
			&dataType,
			&notNull,
			&defaultValue,
			&primaryKeyPosition,
		); err != nil {
			t.Fatal(err)
		}
		if primaryKeyPosition == 1 {
			primaryKeyColumn = name
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if primaryKeyColumn != "name" {
		t.Fatalf("webhook primary key = %q, want name", primaryKeyColumn)
	}

	var referencedTable string
	var sourceColumn string
	var targetColumn string
	err = metadata.db.QueryRowContext(ctx, `
		SELECT "table", "from", "to"
		FROM pragma_foreign_key_list('webhook_deliveries')
		WHERE "from" = 'webhook_name'
	`).Scan(&referencedTable, &sourceColumn, &targetColumn)
	if err != nil {
		t.Fatal(err)
	}
	if referencedTable != "webhooks" || sourceColumn != "webhook_name" || targetColumn != "name" {
		t.Fatalf(
			"delivery foreign key = %s -> %s.%s, want webhook_name -> webhooks.name",
			sourceColumn,
			referencedTable,
			targetColumn,
		)
	}
}

func TestSQLitePutWebhookSerializesConcurrentExplicitUpserts(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	webhooks := []domain.Webhook{
		{
			Name:    "scanner",
			URL:     "https://first.example/hooks",
			Secret:  "first-concurrent-secret",
			Events:  []string{domain.WebhookAssetUploaded},
			Enabled: true,
		},
		{
			Name:    "scanner",
			URL:     "https://second.example/hooks",
			Secret:  "second-concurrent-secret",
			Events:  []string{domain.WebhookAssetDeleted},
			Enabled: true,
		},
	}
	start := make(chan struct{})
	results := make(chan domain.Webhook, len(webhooks))
	errorsByOperation := make(chan error, len(webhooks))
	var workers sync.WaitGroup
	for _, webhook := range webhooks {
		webhook := webhook
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			stored, err := metadata.PutWebhook(ctx, webhook)
			if err != nil {
				errorsByOperation <- err
				return
			}
			results <- stored
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsByOperation)
	for err := range errorsByOperation {
		t.Fatalf("concurrent named webhook upsert failed: %v", err)
	}

	stored, err := metadata.Webhook(ctx, "scanner")
	if err != nil {
		t.Fatal(err)
	}
	for result := range results {
		if result.Name != stored.Name {
			t.Fatalf("upsert returned name %q, final webhook has name %q", result.Name, stored.Name)
		}
	}
	all, err := metadata.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("concurrent upserts created %d webhooks, want 1", len(all))
	}
}

func TestSQLiteOmittedSecretPutPreservesConcurrentExplicitSecret(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	for iteration := 0; iteration < 20; iteration++ {
		name := fmt.Sprintf("scanner-secret-%d", iteration)
		if _, err := metadata.PutWebhook(ctx, domain.Webhook{
			Name:    name,
			URL:     "https://initial.example/hooks",
			Secret:  "initial-concurrent-secret",
			Events:  []string{domain.WebhookAssetUploaded},
			Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := metadata.PutWebhook(ctx, domain.Webhook{
				Name:    name,
				URL:     "https://omitted.example/hooks",
				Events:  []string{domain.WebhookAssetDeleted},
				Enabled: false,
			})
			results <- err
		}()
		go func() {
			<-start
			_, err := metadata.PutWebhook(ctx, domain.Webhook{
				Name:    name,
				URL:     "https://explicit.example/hooks",
				Secret:  "replacement-concurrent-secret",
				Events:  []string{domain.WebhookAssetUploaded},
				Enabled: true,
			})
			results <- err
		}()
		close(start)

		for operation := 0; operation < 2; operation++ {
			if err := <-results; err != nil {
				t.Fatalf("iteration %d: concurrent put failed: %v", iteration, err)
			}
		}
		stored, err := metadata.Webhook(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Secret != "replacement-concurrent-secret" {
			t.Fatalf("iteration %d: omitted put replaced explicit secret", iteration)
		}
	}
}

func TestSQLiteOmittedSecretPutDoesNotRecreateConcurrentlyRemovedName(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	for iteration := 0; iteration < 20; iteration++ {
		name := fmt.Sprintf("scanner-%d", iteration)
		_, err := metadata.PutWebhook(ctx, domain.Webhook{
			Name:    name,
			URL:     "https://initial.example/hooks",
			Secret:  "concurrent-delete-secret",
			Events:  []string{domain.WebhookAssetUploaded},
			Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		putResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() {
			<-start
			_, err := metadata.PutWebhook(ctx, domain.Webhook{
				Name:    name,
				URL:     "https://updated.example/hooks",
				Events:  []string{domain.WebhookAssetDeleted},
				Enabled: true,
			})
			putResult <- err
		}()
		go func() {
			<-start
			deleteResult <- metadata.DeleteWebhook(ctx, name, Ownership{})
		}()
		close(start)

		putErr := <-putResult
		if putErr != nil && !errors.Is(putErr, domain.ErrWebhookSecretRequired) {
			t.Fatalf("iteration %d: omitted-secret put returned %v", iteration, putErr)
		}
		if err := <-deleteResult; err != nil {
			t.Fatalf("iteration %d: delete returned %v", iteration, err)
		}
		if _, err := metadata.Webhook(ctx, name); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("iteration %d: deleted name was recreated: %v", iteration, err)
		}
	}
}

func TestAssetAndUploadEventCommitAtomically(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateWebhook(ctx, domain.Webhook{
		Name: "scanner", URL: "https://scanner.example/events",
		Secret: "scanner-secret-value", Events: []string{domain.WebhookAssetUploaded}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `
		CREATE TRIGGER reject_delivery BEFORE INSERT ON webhook_deliveries
		BEGIN SELECT RAISE(ABORT, 'injected delivery failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "raw", Path: "release.bin",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err == nil {
		t.Fatal("asset publication succeeded after the outbox insert failed")
	}
	if _, err := metadata.Asset(ctx, "raw", "release.bin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("asset survived failed outbox transaction: %v", err)
	}
}

func TestSQLiteWebhookDeliveryLifecycleAndDownloadGate(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "raw",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}

	const webhookSecret = "integration-webhook-secret"
	if err := metadata.CreateWebhook(ctx, domain.Webhook{
		Name:         "scanner",
		URL:          "https://scanner.example/hooks/suxen",
		Secret:       webhookSecret,
		Events:       []string{domain.WebhookAssetUploaded},
		Repositories: []string{"raw"},
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	webhooks, err := metadata.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(webhooks) != 1 {
		t.Fatalf("got %d webhooks, want 1", len(webhooks))
	}
	webhook := webhooks[0]
	webhook.Secret = ""
	if _, err := metadata.PutWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	webhook, err = metadata.Webhook(ctx, webhook.Name)
	if err != nil {
		t.Fatal(err)
	}
	if webhook.Secret != webhookSecret {
		t.Fatal("an update with an omitted secret replaced the webhook secret")
	}

	now := time.Now().UTC()
	payloadAsset := domain.Asset{
		ID:         42,
		Repository: "raw",
		Path:       "release.zip",
	}
	if err := metadata.EnqueueWebhookEvent(ctx, domain.WebhookEvent{
		ID:         "event-1",
		Type:       domain.WebhookAssetUploaded,
		Repository: "raw",
		OccurredAt: now,
		Asset:      &payloadAsset,
	}); err != nil {
		t.Fatal(err)
	}

	claimed, err := metadata.ClaimWebhookDeliveries(
		ctx,
		"worker-a",
		now,
		30*time.Second,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("unexpected first delivery claim: %+v", claimed)
	}
	if claimed[0].WebhookName != webhook.Name {
		t.Fatalf("delivery webhook name = %q, want %q", claimed[0].WebhookName, webhook.Name)
	}
	if claimed[0].Secret != webhookSecret {
		t.Fatal("claimed delivery did not include its signing secret")
	}
	var event domain.WebhookEvent
	if err := json.Unmarshal(claimed[0].Payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Asset == nil || event.Asset.Path != payloadAsset.Path {
		t.Fatalf("unexpected webhook payload: %+v", event)
	}

	duplicateClaim, err := metadata.ClaimWebhookDeliveries(
		ctx,
		"worker-b",
		now,
		30*time.Second,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicateClaim) != 0 {
		t.Fatalf("a second worker claimed leased delivery: %+v", duplicateClaim)
	}

	retryAt := now.Add(time.Minute)
	if err := metadata.CompleteWebhookDelivery(
		ctx,
		claimed[0].ID,
		"worker-a",
		"retry",
		retryAt,
		"temporary failure",
	); err != nil {
		t.Fatal(err)
	}
	claimed, err = metadata.ClaimWebhookDeliveries(
		ctx,
		"worker-b",
		retryAt,
		30*time.Second,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("retry was not claimed exactly once: %+v", claimed)
	}
	if err := metadata.CompleteWebhookDelivery(
		ctx,
		claimed[0].ID,
		"worker-b",
		"delivered",
		retryAt,
		"",
	); err != nil {
		t.Fatal(err)
	}
	deliveries, err := metadata.WebhookDeliveries(ctx, webhook.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != "delivered" ||
		deliveries[0].DeliveredAt == nil {
		t.Fatalf("unexpected delivery history: %+v", deliveries)
	}

	gate := domain.DownloadGate{
		Repository: "raw",
		Criteria: []domain.Predicate{
			{Path: "scan.status", Op: "=", Value: "passed"},
		},
		Enabled: true,
	}
	if err := metadata.SetDownloadGate(ctx, gate); err != nil {
		t.Fatal(err)
	}
	storedGate, err := metadata.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if len(storedGate.Criteria) != 1 || storedGate.Criteria[0].Value != "passed" || !storedGate.Enabled {
		t.Fatalf("unexpected stored download gate: %+v", storedGate)
	}
	if err := metadata.DeleteDownloadGate(ctx, "raw", Ownership{}); err != nil {
		t.Fatal(err)
	}

	trustPolicy := domain.TrustPolicy{
		Repository: "raw",
		Mode:       "audit",
		PublicKeys: []string{"test-public-key"},
	}
	if err := metadata.SetTrustPolicy(ctx, trustPolicy); err != nil {
		t.Fatal(err)
	}
	storedPolicy, err := metadata.TrustPolicy(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if storedPolicy.Mode != trustPolicy.Mode || storedPolicy.UpdatedAt.IsZero() {
		t.Fatalf("unexpected stored trust policy: %+v", storedPolicy)
	}
	if err := metadata.DeleteTrustPolicy(ctx, "raw", Ownership{}); err != nil {
		t.Fatal(err)
	}
}
