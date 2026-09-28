package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestPairedSQLiteFilesystemBackupRestoresArtifact(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	metadataPath := filepath.Join(source, "metadata.db")
	metadata, err := OpenSQLite(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := createTestDefaultBlobStore(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "releases", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	blobRoot := filepath.Join(source, "blobs")
	blobStore, err := blob.NewFS(blobRoot)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("paired backup artifact")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	if _, err := blobStore.Put(ctx, digest, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "releases", Path: "v1/artifact.bin", Digest: digest,
		Size: int64(len(payload)), ContentType: "application/octet-stream",
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	restored := t.TempDir()
	copyTree(t, metadataPath, filepath.Join(restored, "metadata.db"))
	copyTree(t, blobRoot, filepath.Join(restored, "blobs"))
	restoredMetadata, err := OpenSQLite(filepath.Join(restored, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restoredMetadata.Close() })
	if err := restoredMetadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	asset, err := restoredMetadata.Asset(ctx, "releases", "v1/artifact.bin")
	if err != nil {
		t.Fatal(err)
	}
	restoredBlobs, err := blob.NewFS(filepath.Join(restored, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := restoredBlobs.Get(ctx, asset.Digest)
	if err != nil {
		t.Fatal(err)
	}
	read, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(read, payload) {
		t.Fatalf("restored artifact = %q, want %q", read, payload)
	}
}

func copyTree(t *testing.T, source string, target string) {
	t.Helper()
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		input, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(output, input); err != nil {
			_ = output.Close()
			t.Fatal(err)
		}
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o750)
		}
		copyTree(t, path, destination)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
