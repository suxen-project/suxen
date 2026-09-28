package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// A repository publication stalled after validating its destination must keep
// that destination active until the insert or rebind commits. Otherwise a
// concurrent drain can finish first and strand the repository on that store.
func TestPostgresRepositoryPublicationSerializesWithBlobStoreDrain(t *testing.T) {
	for _, create := range []bool{true, false} {
		name := "create"
		if !create {
			name = "rebind"
		}
		t.Run(name, func(t *testing.T) {
			testPostgresRepositoryPublicationDrain(t, create)
		})
	}
}

func testPostgresRepositoryPublicationDrain(t *testing.T, create bool) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx := context.Background()
	createDrainTestBlobStore(t, metadata, "source", "1")
	createDrainTestBlobStore(t, metadata, "target", "2")
	if !create {
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: "new", Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The table lock lets SaveRepository pass its blob-store check but holds
	// its INSERT or UPDATE. Observe the actual blocked write before the drain.
	blocker, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `LOCK TABLE repositories IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	saveCtx, cancelSave := context.WithCancel(ctx)
	defer cancelSave()
	saved := make(chan error, 1)
	joined := false
	defer func() {
		if joined {
			return
		}
		cancelSave()
		_ = blocker.Rollback()
		select {
		case <-saved:
		case <-time.After(10 * time.Second):
			t.Error("repository save did not stop during cleanup")
		}
	}()
	go func() {
		saved <- metadata.SaveRepository(saveCtx, RepositorySave{
			Repository: domain.Repository{
				Name: "new", Format: "raw", Type: "hosted", BlobStore: "source",
			},
			Create: create,
		})
	}()
	waitForRepositoryWriteWaiter(t, metadata)

	drainCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err = metadata.BeginBlobStoreDrain(drainCtx, "source", "target")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain while repository publication is pending = %v, want blocked until publication commits", err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, saved); err != nil {
		t.Fatalf("repository publication after blocker release: %v", err)
	}
	joined = true
	if err := metadata.BeginBlobStoreDrain(ctx, "source", "target"); err != nil {
		t.Fatalf("drain after repository publication: %v", err)
	}
}

func waitForRepositoryWriteWaiter(t *testing.T, metadata *SQLStore) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiters int
		err := metadata.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE relation = 'repositories'::regclass
			   AND mode = 'RowExclusiveLock' AND NOT granted`,
		).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for repository write")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
