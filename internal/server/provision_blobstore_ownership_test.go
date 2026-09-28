package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/store"
)

// putProvisionRecordSpy counts generic ownership-record writes per kind so a
// test can assert that a kind which folds its ownership record into its own
// transaction never falls back to the engine's generic PutProvisionRecord.
type putProvisionRecordSpy struct {
	store.Store
	blobStoreWrites atomic.Int32
}

func (s *putProvisionRecordSpy) PutProvisionRecord(ctx context.Context, record store.ProvisionRecord) error {
	if record.Kind == "blobStore" {
		s.blobStoreWrites.Add(1)
	}
	return s.Store.PutProvisionRecord(ctx, record)
}

// TestProvisionBlobStoreUnchangedFoldsOwnershipRecord drives an equal-definition
// reconcile through the real provisionBlobStoreController and Engine.reconcile
// dispatch, proving the ownership record is re-established through the blob-store
// command port (inside the keyed ownership lock) and not through the engine's
// generic writer.
//
// The generic writer runs outside that lock, so a forced API transfer between
// the equality read and the record write could leave the row holding the API
// value while a freshly recreated record still claims the store is provisioned.
// The fix routes the unchanged-definition effect through SaveBlobStore and
// reports it persisted, so the engine skips PutProvisionRecord entirely. A store
// lock test cannot catch a regression here: SaveBlobStore already takes the lock,
// so it would pass even if the controller returned "not persisted" and the engine
// used the generic path. This test fails in exactly that case.
func TestProvisionBlobStoreUnchangedFoldsOwnershipRecord(t *testing.T) {
	handler := newDrainTestHandler(t)
	ctx := context.Background()

	document := provision.Document{
		APIVersion: provision.APIVersion,
		Resources: []provision.Resource{{
			Kind: "blobStore",
			Name: "secondary",
			Spec: map[string]any{
				"driver":           "tracking",
				"configurationRef": map[string]any{"env": "SUXEN_TEST_SECONDARY_STORE"},
			},
		}},
	}

	// Create the managed blob store: the row and its ownership record are folded
	// into one transaction by the command port.
	created, err := handler.provisionEngine().Apply(ctx, document, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Failed() {
		t.Fatalf("create report failed: %+v", created.Results)
	}
	if _, err := handler.metadata.ProvisionRecord(ctx, "blobStore", "secondary"); err != nil {
		t.Fatalf("ownership record missing after create: %v", err)
	}

	// Drop the ownership record so the next reconcile — an unchanged definition —
	// must re-establish it. Whichever path recreates it is now observable: the
	// owned save re-puts it under the lock, the generic writer would count on the
	// spy.
	if err := handler.metadata.DeleteProvisionRecord(ctx, "blobStore", "secondary"); err != nil {
		t.Fatal(err)
	}

	engine := handler.provisionEngine()
	spy := &putProvisionRecordSpy{Store: engine.Store}
	engine.Store = spy

	report, err := engine.Apply(ctx, document, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed() {
		t.Fatalf("unchanged reconcile failed: %+v", report.Results)
	}
	unchanged := false
	for _, result := range report.Results {
		if result.Kind == "blobStore" && result.Name == "secondary" {
			unchanged = result.Status == provision.StatusUnchanged
		}
	}
	if !unchanged {
		t.Fatalf("blob store reconcile status = %+v, want unchanged", report.Results)
	}
	if _, err := handler.metadata.ProvisionRecord(ctx, "blobStore", "secondary"); err != nil {
		t.Fatalf("ownership record not re-established by the command port: %v", err)
	}
	if writes := spy.blobStoreWrites.Load(); writes != 0 {
		t.Fatalf("generic PutProvisionRecord used for blob store %d time(s); the owned save must persist the record", writes)
	}
}
