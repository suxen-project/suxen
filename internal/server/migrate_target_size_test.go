package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

// A pre-existing target key can be incomplete even though Head reports it as
// present. Migration must retain the intact source until the target is repaired.
func TestMigrationKeepsSourceWhenTargetBlobHasWrongSize(t *testing.T) {
	ctx := context.Background()
	source, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	target, err := blob.NewFS(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete source blob")
	digest := testDigest(payload)
	for _, store := range []*blob.FS{source, target} {
		if _, err := store.Put(ctx, digest, bytes.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
	}
	hash := strings.TrimPrefix(digest, "sha256:")
	targetPath := filepath.Join(targetRoot, "sha256", hash[:2], hash[2:4], hash)
	if err := os.WriteFile(targetPath, []byte("truncated"), 0o640); err != nil {
		t.Fatal(err)
	}
	copied, copyErr := copyMissingBlobs(ctx, source, target)
	deleted, _, drainErr := drainSourceBlobs(ctx, source, target)
	_, sourceErr := source.Head(ctx, digest)
	if copyErr == nil || copied != 0 || drainErr == nil || deleted != 0 || sourceErr != nil {
		t.Fatalf("truncated target: copy = %d/%v, drain = %d/%v, source = %v; want both operations to fail with source retained",
			copied, copyErr, deleted, drainErr, sourceErr)
	}

	// Once the bad target is removed, a retry copies the intact source and can
	// safely finish draining it.
	if err := target.Delete(ctx, digest); err != nil {
		t.Fatal(err)
	}
	if copied, err := copyMissingBlobs(ctx, source, target); err != nil || copied != 1 {
		t.Fatalf("copy after target repair = %d, %v", copied, err)
	}
	reader, _, err := target.Get(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("repaired target content = %q, read error %v, close error %v", got, readErr, closeErr)
	}
	if deleted, remaining, err := drainSourceBlobs(ctx, source, target); err != nil || deleted != 1 || remaining != 0 {
		t.Fatalf("drain after target repair = deleted %d, remaining %d, error %v", deleted, remaining, err)
	}
	if _, err := source.Head(ctx, digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("drained source still has blob: %v", err)
	}
}
