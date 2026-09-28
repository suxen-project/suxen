package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestPostgresRepositoryTransferAndPruneSerialize proves the atomic-boundary HA
// behavior the repository ownership fold introduces: a prune (a declarative
// DeleteRepository) and a concurrent ownership change serialize on the
// repository's ownership advisory key, and the prune re-reads the ownership
// record after the wait rather than acting on the plan it captured earlier. This
// is the behavior the old two-step delete needed a separate re-check for; the
// serial ownership test cannot observe it. SQLite serializes writers implicitly,
// so this is a PostgreSQL advisory-lock guarantee (openCompanionTestStore skips
// otherwise). It is deterministic rather than timing-dependent: a controlling
// transaction holds the repository key while the prune blocks on it (confirmed
// through pg_locks) and commits the competing state before releasing the lock.
func TestPostgresRepositoryTransferAndPruneSerialize(t *testing.T) {
	t.Run("forced API transfer before prune deletes: prune skips, repository survives", func(t *testing.T) {
		metadata := openCompanionTestStore(t, "postgres")
		ctx := context.Background()
		mustSaveManagedRepository(t, metadata, "app")

		controller, err := metadata.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Rollback()
		if err := lockOwnershipTx(ctx, controller, "repository", "app"); err != nil {
			t.Fatal(err)
		}

		// The prune's declarative delete blocks on the ownership key.
		result := make(chan error, 1)
		go func() { result <- metadata.DeleteRepository(ctx, "app", Ownership{Declarative: true}) }()
		waitForOwnershipKeyWaiter(t, metadata, "repository", "app")

		// A forced API transfer relinquishes provisioning ownership (drops the
		// record) while the prune is already blocked on the key.
		if _, err := controller.ExecContext(ctx,
			`DELETE FROM provision_records WHERE kind = ? AND name = ?`, "repository", "app",
		); err != nil {
			t.Fatal(err)
		}
		if err := controller.Commit(); err != nil {
			t.Fatal(err)
		}

		// The prune must skip: the record is gone, so the repository is now
		// API-owned and must be left in place.
		if err := awaitResult(t, result); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("prune after concurrent transfer = %v, want ErrNotFound (skip)", err)
		}
		if _, err := metadata.Repository(ctx, "app"); err != nil {
			t.Fatalf("prune deleted a transferred repository: %v", err)
		}
		if _, err := metadata.ProvisionRecord(ctx, "repository", "app"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("ownership record unexpectedly present: %v", err)
		}
	})

	t.Run("re-adoption before prune deletes: prune deletes the current repository", func(t *testing.T) {
		metadata := openCompanionTestStore(t, "postgres")
		ctx := context.Background()
		mustSaveManagedRepository(t, metadata, "app")

		controller, err := metadata.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Rollback()
		if err := lockOwnershipTx(ctx, controller, "repository", "app"); err != nil {
			t.Fatal(err)
		}

		result := make(chan error, 1)
		go func() { result <- metadata.DeleteRepository(ctx, "app", Ownership{Declarative: true}) }()
		waitForOwnershipKeyWaiter(t, metadata, "repository", "app")

		// A concurrent apply re-adopts the repository (refreshes the record) while
		// the prune is already blocked on the key.
		if _, err := controller.ExecContext(ctx,
			`UPDATE provision_records SET secret_fingerprint = ?, updated_at = ? WHERE kind = ? AND name = ?`,
			"readopted", formatTime(time.Now().UTC()), "repository", "app",
		); err != nil {
			t.Fatal(err)
		}
		if err := controller.Commit(); err != nil {
			t.Fatal(err)
		}

		// The record is present at the atomic delete point, so the prune deletes
		// the repository and its record together — the outcome is decided under the
		// key, not by the earlier plan. (Concurrent provisioning applies cannot
		// actually interleave: the declarative-provisioning lease serializes them;
		// the forced-API-transfer path above is what protects API ownership.)
		if err := awaitResult(t, result); err != nil {
			t.Fatalf("prune with a present record = %v, want success", err)
		}
		if _, err := metadata.Repository(ctx, "app"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("prune left the repository: %v", err)
		}
		if _, err := metadata.ProvisionRecord(ctx, "repository", "app"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("prune left the ownership record: %v", err)
		}
	})
}

func mustSaveManagedRepository(t *testing.T, metadata *SQLStore, name string) {
	t.Helper()
	if err := metadata.SaveRepository(context.Background(), RepositorySave{
		Repository: domain.Repository{Name: name, Format: "raw", Type: "hosted", Writable: true},
		Create:     true,
		Ownership:  Ownership{Declarative: true, Fingerprint: "fp"},
	}); err != nil {
		t.Fatal(err)
	}
}

// waitForOwnershipKeyWaiter blocks until a backend is waiting for the resource's
// ownership advisory lock, so the interleaving is fixed by lock state rather than
// by timing. The single-argument advisory lock splits its key into classid (high
// 32 bits) and objid (low 32 bits) with objsubid 1.
func waitForOwnershipKeyWaiter(t *testing.T, metadata *SQLStore, kind, name string) {
	t.Helper()
	ctx := context.Background()
	key := uint64(ownershipLockKey(kind, name))
	classID := int64(key >> 32)
	objID := int64(key & 0xFFFFFFFF)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiters int
		err := metadata.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND classid::bigint = ? AND objid::bigint = ?
			   AND objsubid = 1 AND NOT granted`,
			classID, objID).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the repository advisory-lock waiter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
