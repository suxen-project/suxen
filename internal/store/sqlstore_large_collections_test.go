package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestSQLiteTaskAndDeliveryListsExceedOneThousand(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateWebhook(ctx, domain.Webhook{
		Name: "large-history", URL: "https://hooks.example/events",
		Secret: "large-history-test-secret", Events: []string{domain.WebhookAssetUploaded}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	webhooks, err := metadata.Webhooks(ctx)
	if err != nil || len(webhooks) != 1 {
		t.Fatalf("resolve webhook: %v, %+v", err, webhooks)
	}

	transaction, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	now := formatTime(time.Now().UTC())
	for index := 0; index < 1001; index++ {
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO tasks (type, status, result, created_at) VALUES (?, ?, '{}', ?)`,
			"test", "complete", now,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO webhook_deliveries (
				webhook_name, event, repository, payload, status,
				next_attempt_at, created_at, updated_at
			) VALUES (?, ?, ?, '{}', 'queued', ?, ?, ?)`,
			webhooks[0].Name, domain.WebhookAssetUploaded, "raw", now, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}

	taskPage, err := metadata.TaskPage(ctx, 0, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.CreateTask(ctx, domain.Task{Type: "new", Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	taskCount := len(taskPage.Items)
	for taskPage.HasMore {
		beforeID := taskPage.Items[len(taskPage.Items)-1].ID
		taskPage, err = metadata.TaskPage(ctx, taskPage.SnapshotID, beforeID, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(taskPage.Items) > 200 {
			t.Fatalf("task page exceeded its bound: %d", len(taskPage.Items))
		}
		taskCount += len(taskPage.Items)
	}
	if taskCount != 1001 {
		t.Fatalf("task snapshot returned %d entries, want 1001", taskCount)
	}

	deliveryPage, err := metadata.WebhookDeliveryPage(ctx, webhooks[0].Name, 0, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(
		ctx,
		`INSERT INTO webhook_deliveries (
			webhook_name, event, repository, payload, status,
			next_attempt_at, created_at, updated_at
		) VALUES (?, ?, ?, '{}', 'queued', ?, ?, ?)`,
		webhooks[0].Name, domain.WebhookAssetUploaded, "raw", now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	deliveryCount := len(deliveryPage.Items)
	for deliveryPage.HasMore {
		beforeID := deliveryPage.Items[len(deliveryPage.Items)-1].ID
		deliveryPage, err = metadata.WebhookDeliveryPage(
			ctx, webhooks[0].Name, deliveryPage.SnapshotID, beforeID, 200,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(deliveryPage.Items) > 200 {
			t.Fatalf("delivery page exceeded its bound: %d", len(deliveryPage.Items))
		}
		deliveryCount += len(deliveryPage.Items)
	}
	if deliveryCount != 1001 {
		t.Fatalf("delivery snapshot returned %d entries, want 1001", deliveryCount)
	}
}
