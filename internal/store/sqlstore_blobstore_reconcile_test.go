package store

import (
	"context"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestPostgresBlobStoreUnchangedReconcileSerialize covers the race the blob-store
// fold's reconcile path must close: when the stored definition already equals the
// desired one, the ownership effect is still applied through the owned save under
// the ("blobStore", name) key, not by a generic post-commit writer outside it. A
// forced API transfer that releases ownership between the reconcile's equality
// read and its ownership write therefore serializes on the key rather than racing
// a lock-free record write, so the row and record can never end torn (row holding
// the API value while a recreated record claims it is provisioned).
//
// It is deterministic: a controlling transaction holds the key while the owned
// save blocks on it (confirmed through pg_locks) and commits the transfer before
// releasing. SQLite serializes writers implicitly, so this is a PostgreSQL
// advisory-lock guarantee (openCompanionTestStore skips otherwise).
func TestPostgresBlobStoreUnchangedReconcileSerialize(t *testing.T) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx := context.Background()

	blobStore := domain.BlobStore{
		Name:             "archive",
		Driver:           "fs",
		ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_ARCHIVE"},
		PhysicalIdentity: strings.Repeat("a", 64),
	}
	if err := metadata.SaveBlobStore(ctx, BlobStoreSave{
		BlobStore: blobStore,
		Create:    true,
		Ownership: Ownership{Declarative: true, Fingerprint: "fp"},
	}); err != nil {
		t.Fatal(err)
	}

	controller, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Rollback()
	if err := lockOwnershipTx(ctx, controller, "blobStore", "archive"); err != nil {
		t.Fatal(err)
	}

	// The reconcile of an unchanged definition applies its ownership effect through
	// the owned save (an identical-definition update is a row no-op), which blocks
	// on the key.
	result := make(chan error, 1)
	go func() {
		result <- metadata.SaveBlobStore(ctx, BlobStoreSave{
			BlobStore: blobStore,
			Create:    false,
			Ownership: Ownership{Declarative: true, Fingerprint: "fp"},
		})
	}()
	waitForOwnershipKeyWaiter(t, metadata, "blobStore", "archive")

	// A forced API transfer releases ownership while the reconcile save is blocked.
	if _, err := controller.ExecContext(ctx,
		`DELETE FROM provision_records WHERE kind = ? AND name = ?`, "blobStore", "archive",
	); err != nil {
		t.Fatal(err)
	}
	if err := controller.Commit(); err != nil {
		t.Fatal(err)
	}

	// The reconcile save then re-adopts atomically under the key. The outcome is
	// consistent — the record is present and matches the row — never torn.
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("reconcile owned save = %v, want success", err)
	}
	record, err := metadata.ProvisionRecord(ctx, "blobStore", "archive")
	if err != nil {
		t.Fatalf("reconcile did not re-adopt the record under the key: %v", err)
	}
	if record.SecretFingerprint != "fp" {
		t.Fatalf("record fingerprint = %q, want fp", record.SecretFingerprint)
	}
	if _, err := metadata.BlobStore(ctx, "archive"); err != nil {
		t.Fatalf("blob store row missing: %v", err)
	}
}
