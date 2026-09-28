package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type failBlobMetadataDeleteOnce struct {
	store.Store
	failure error
}

func (metadata *failBlobMetadataDeleteOnce) DeleteUnreferencedBlobAssets(
	ctx context.Context, blobStoreName string, digest string,
) (int64, error) {
	if metadata.failure != nil {
		err := metadata.failure
		metadata.failure = nil
		return 0, err
	}
	return metadata.Store.DeleteUnreferencedBlobAssets(ctx, blobStoreName, digest)
}

func TestGCMetadataFailureKeepsBlobReadableAndRetryable(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	configuredStore, err := fixture.Handler.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("unreferenced OCI upload")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := configuredStore.Put(ctx, digest, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	asset, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci", Path: "app/blobs/" + digest,
		Digest: digest, Size: int64(len(payload)), Kind: "oci-blob",
	})
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("metadata write unavailable")
	fixture.Handler.metadata = &failBlobMetadataDeleteOnce{
		Store: fixture.Metadata, failure: injected,
	}
	// Use a future cutoff so the newly created file is eligible without relying
	// on wall-clock timing or filesystem timestamp resolution.
	now := time.Now().Add(time.Hour)
	_, err = fixture.Handler.collectGarbage(ctx, false, 0, now, "default")
	if !errors.Is(err, injected) {
		t.Fatalf("GC metadata failure = %v, want injected error", err)
	}
	if _, err := configuredStore.Head(ctx, digest); err != nil {
		t.Fatalf("metadata failure deleted the still-indexed physical blob: %v", err)
	}
	if _, err := fixture.Metadata.AssetByID(ctx, "oci", asset.ID); err != nil {
		t.Fatalf("metadata failure removed the OCI blob asset: %v", err)
	}

	result, err := fixture.Handler.collectGarbage(ctx, false, 0, now, "default")
	if err != nil {
		t.Fatalf("retry GC: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("retry deleted %d blobs, want 1", result.Deleted)
	}
	if _, err := configuredStore.Head(ctx, digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("retry left physical blob: %v", err)
	}
	if _, err := fixture.Metadata.AssetByID(ctx, "oci", asset.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("retry left unreferenced OCI metadata: %v", err)
	}
}
