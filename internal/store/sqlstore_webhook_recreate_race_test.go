package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// An event selected for a webhook must not be attached to a new subscription
// created later under the same name. The test trigger pauses delivery insertion
// after subscription selection without changing production code.
func TestPostgresWebhookEnqueueDoesNotReachRecreatedSubscription(t *testing.T) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	webhook := domain.Webhook{
		Name: "scanner", URL: "https://old.example/hooks", Secret: "old-webhook-secret",
		Events: []string{domain.WebhookAssetDeleted}, Repositories: []string{"raw"}, Enabled: true,
	}
	if err := metadata.CreateWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	key := ownershipLockKey("webhook-fanout-test", webhook.Name)
	trigger := fmt.Sprintf(`
		CREATE FUNCTION wait_before_delivery_insert() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%d);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER wait_before_delivery_insert
		BEFORE INSERT ON webhook_deliveries
		FOR EACH ROW EXECUTE FUNCTION wait_before_delivery_insert();`, key)
	if _, err := metadata.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	controller, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Rollback()
	if _, err := controller.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, key); err != nil {
		t.Fatal(err)
	}

	enqueued := make(chan error, 1)
	mutated := make(chan error, 1)
	enqueueJoined, mutationJoined := false, false
	defer func() {
		cancel()
		_ = controller.Rollback()
		if !enqueueJoined {
			select {
			case <-enqueued:
			case <-time.After(10 * time.Second):
				t.Error("webhook enqueue did not stop during cleanup")
			}
		}
		if !mutationJoined {
			select {
			case <-mutated:
			case <-time.After(10 * time.Second):
				t.Error("webhook mutation did not stop during cleanup")
			}
		}
	}()
	go func() {
		enqueued <- metadata.EnqueueWebhookEvent(ctx, domain.WebhookEvent{
			ID: "old-event", Type: domain.WebhookAssetDeleted,
			Repository: "raw", OccurredAt: time.Now().UTC(),
		})
	}()
	waitForOwnershipKeyWaiter(t, metadata, "webhook-fanout-test", webhook.Name)
	go func() {
		if err := metadata.DeleteWebhook(ctx, webhook.Name, Ownership{}); err != nil {
			mutated <- err
			return
		}
		webhook.URL = "https://new.example/hooks"
		webhook.Secret = "new-webhook-secret"
		mutated <- metadata.CreateWebhook(ctx, webhook)
	}()
	waitForWebhookDeleteWaiter(t, metadata, mutated, &mutationJoined)
	if err := controller.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, enqueued); err != nil {
		t.Fatalf("enqueue event: %v", err)
	}
	enqueueJoined = true
	if err := awaitResult(t, mutated); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}
	mutationJoined = true
	deliveries, err := metadata.WebhookDeliveries(ctx, webhook.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("replacement webhook inherited %d delivery rows from deleted subscription", len(deliveries))
	}
}

func waitForWebhookDeleteWaiter(t *testing.T, metadata *SQLStore, mutated <-chan error, mutationJoined *bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-mutated:
			*mutationJoined = true
			t.Fatalf("webhook delete/recreate completed while old event insert was pending: %v", err)
		default:
		}
		var waiters int
		err := metadata.db.QueryRowContext(context.Background(), `
			SELECT count(*) FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.query LIKE '%DELETE FROM webhooks WHERE name%'`,
		).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for webhook delete to wait on the fanout transaction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
