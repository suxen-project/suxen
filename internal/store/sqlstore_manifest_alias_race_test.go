package store

import (
	"context"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// A tag publication writes its tag before its canonical digest alias. If it
// waits on the alias row, pruning the previous tag must observe the new tag
// after that publication commits instead of deleting the canonical alias.
func TestPostgresManifestAliasPruneSerializesWithPublication(t *testing.T) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "images", Format: "oci", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:8888888888888888888888888888888888888888888888888888888888888888"
	path := "v2/example/image/manifests/"
	manifest := func(reference string) domain.Asset {
		return domain.Asset{
			Repository: "images", Path: path + reference, Digest: digest,
			Kind: "oci-manifest", Reference: reference, Size: 1,
		}
	}
	if _, err := metadata.PutAssets(ctx, []domain.Asset{manifest("old"), manifest(digest)}); err != nil {
		t.Fatal(err)
	}
	removed, err := metadata.DeleteAsset(ctx, "images", path+"old")
	if err != nil {
		t.Fatal(err)
	}

	controller, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Rollback()
	var lockedID int64
	if err := controller.QueryRowContext(ctx,
		`SELECT id FROM assets WHERE path = ? FOR UPDATE`, path+digest,
	).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	pruned := make(chan error, 1)
	publisherStarted, prunerStarted := false, false
	publisherJoined, prunerJoined := false, false
	defer func() {
		cancel()
		_ = controller.Rollback()
		if publisherStarted && !publisherJoined {
			select {
			case <-published:
			case <-time.After(10 * time.Second):
				t.Error("publisher did not stop")
			}
		}
		if prunerStarted && !prunerJoined {
			select {
			case <-pruned:
			case <-time.After(10 * time.Second):
				t.Error("pruner did not stop")
			}
		}
	}()
	go func() {
		_, err := metadata.PutAssets(ctx, []domain.Asset{manifest("new"), manifest(digest)})
		published <- err
	}()
	publisherStarted = true
	waitForManifestAliasLockWaiter(t, metadata, "SELECT id, digest, last_accessed FROM assets")
	go func() {
		_, err := metadata.DeleteDanglingManifestAliases(ctx, "images", []domain.Asset{removed})
		pruned <- err
	}()
	prunerStarted = true
	waitForManifestAliasPruneWaiter(t, metadata)
	if err := controller.Commit(); err != nil {
		t.Fatal(err)
	}
	publicationErr := <-published
	publisherJoined = true
	if publicationErr != nil {
		t.Fatal(publicationErr)
	}
	pruneErr := <-pruned
	prunerJoined = true
	if pruneErr != nil {
		t.Fatal(pruneErr)
	}
	if _, err := metadata.Asset(ctx, "images", path+"new"); err != nil {
		t.Fatalf("new tag was lost: %v", err)
	}
	if _, err := metadata.Asset(ctx, "images", path+digest); err != nil {
		t.Fatalf("new tag lost its canonical digest alias: %v", err)
	}
}

func waitForManifestAliasLockWaiter(t *testing.T, metadata *SQLStore, queryFragment string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		err := metadata.db.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT activity.pid) FROM pg_stat_activity AS activity
			 JOIN pg_locks AS relation_lock ON relation_lock.pid = activity.pid
			 JOIN pg_locks AS wait_lock ON wait_lock.pid = activity.pid
			 WHERE relation_lock.relation = 'assets'::regclass AND relation_lock.granted
			   AND NOT wait_lock.granted AND activity.wait_event_type = 'Lock'
			   AND activity.query LIKE ?`,
			"%"+queryFragment+"%",
		).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q to lock", queryFragment)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForManifestAliasPruneWaiter(t *testing.T, metadata *SQLStore) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		err := metadata.db.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT activity.pid) FROM pg_stat_activity AS activity
			 JOIN pg_locks AS relation_lock ON relation_lock.pid = activity.pid
			 JOIN pg_locks AS wait_lock ON wait_lock.pid = activity.pid
			 WHERE relation_lock.relation = 'assets'::regclass AND relation_lock.granted
			   AND NOT wait_lock.granted AND activity.wait_event_type = 'Lock'
			   AND (activity.query LIKE '%DELETE FROM assets%'
			     OR activity.query LIKE '%SELECT id, digest FROM assets%')`,
		).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for manifest alias prune")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
