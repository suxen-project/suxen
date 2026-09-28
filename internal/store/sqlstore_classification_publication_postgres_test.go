package store

import (
	"context"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Concurrent publishers take compatible shared locks, while a relabel must
// wait for those publishers before changing the rules and existing asset labels.
func TestPostgresClassificationPublicationLocks(t *testing.T) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	first, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if err := lockClassificationPublication(ctx, first); err != nil {
		t.Fatal(err)
	}

	// The first publication keeps its lock while a second publication commits.
	// An exclusive lock here would make the second publication time out.
	publishContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = metadata.PutAsset(publishContext, domain.Asset{
		Repository: "raw", Path: "second.bin",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
	})
	if err != nil {
		t.Fatalf("concurrent publication was blocked by another publisher: %v", err)
	}

	contender, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Rollback()
	var acquired bool
	if err := contender.QueryRowContext(ctx,
		`SELECT pg_try_advisory_xact_lock(?)`, ownershipLockKey(classificationRelabelLock, ""),
	).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("relabel acquired exclusive lock while publication was still active")
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := lockClassificationRelabel(ctx, contender); err != nil {
		t.Fatalf("relabel could not acquire lock after publication committed: %v", err)
	}
}
