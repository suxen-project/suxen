package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestFSPutGetAndDeduplicate(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := "repeatable artifact"
	digest := digestForTest(content)

	first, err := store.Put(context.Background(), digest, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), digest, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("deduplicated blob metadata changed: first=%+v second=%+v", first, second)
	}

	reader, info, err := store.Get(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != content {
		t.Fatalf("got %q, want %q", actual, content)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("got size %d, want %d", info.Size, len(content))
	}
}

func TestFSPutRejectsDigestMismatch(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	wrongDigest := digestForTest("different content")
	_, err = store.Put(context.Background(), wrongDigest, strings.NewReader("artifact"))
	if !errors.Is(err, domain.ErrDigestMismatch) {
		t.Fatalf("got error %v, want ErrDigestMismatch", err)
	}
}

func TestFSWalkSkipsNoncanonicalDigestShards(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	canonical := digestForTest("canonical")
	if _, err := store.Put(ctx, canonical, strings.NewReader("canonical")); err != nil {
		t.Fatal(err)
	}
	stray := digestForTest("misplaced")
	strayPath := filepath.Join(store.root, "sha256", "wrong-shard", strings.TrimPrefix(stray, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(strayPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strayPath, []byte("misplaced"), 0o640); err != nil {
		t.Fatal(err)
	}
	linked := digestForTest("linked")
	linkPath, err := store.path(linked)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath, err := store.path(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o750); err != nil {
		t.Fatal(err)
	}
	// Some platforms restrict symlink creation. The misplaced regular file
	// above still exercises canonical shard filtering there.
	if err := os.Symlink(canonicalPath, linkPath); err != nil {
		t.Logf("canonical-path symlink check unavailable: %v", err)
	}
	var walked []string
	if err := store.Walk(ctx, func(info domain.BlobInfo) error {
		walked = append(walked, info.Digest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || walked[0] != canonical {
		t.Fatalf("Walk yielded %v, want only canonical blob %s", walked, canonical)
	}
}

func TestFSWalkPreservesMissingFileCallbackError(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, digestForTest("source"), strings.NewReader("source")); err != nil {
		t.Fatal(err)
	}
	want := fmt.Errorf("migration target write failed: %w", os.ErrNotExist)
	err = store.Walk(ctx, func(domain.BlobInfo) error { return want })
	if err != want {
		t.Fatalf("Walk error = %v, want callback error %v", err, want)
	}
}

func TestFSWalkContinuesAfterEnumeratedBlobDisappears(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Place three canonical filenames in one shard so WalkDir enumerates them
	// together. Walk inspects filenames and metadata, not content digests.
	digests := make([]string, 3)
	for index := range digests {
		digests[index] = "sha256:" + strings.Repeat("a", 63) + fmt.Sprint(index)
		file, err := store.path(digests[index])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("content"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var seen []string
	err = store.Walk(ctx, func(info domain.BlobInfo) error {
		seen = append(seen, info.Digest)
		if info.Digest == digests[0] {
			return store.Delete(ctx, digests[1])
		}
		return nil
	})
	if err != nil || len(seen) != 2 || seen[0] != digests[0] || seen[1] != digests[2] {
		t.Fatalf("Walk after concurrent deletion = %v, %v", seen, err)
	}
}

func TestFSUploadSessionLifecycleAndLimitRollback(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "oci/repository/session"
	if err := store.CreateUpload(ctx, key); err != nil {
		t.Fatal(err)
	}
	if size, err := store.AppendUpload(ctx, key, strings.NewReader("first"), 10); err != nil || size != 5 {
		t.Fatalf("first append: size=%d err=%v", size, err)
	}
	if size, err := store.AppendUpload(ctx, key, strings.NewReader("-too-long"), 10); !errors.Is(err, ErrUploadTooLarge) || size != 5 {
		t.Fatalf("oversized append: size=%d err=%v", size, err)
	}

	reader, size, err := store.OpenUpload(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read upload: read=%v close=%v", readErr, closeErr)
	}
	if size != 5 || string(content) != "first" {
		t.Fatalf("rolled-back upload: size=%d content=%q", size, content)
	}
	if err := store.DeleteUpload(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OpenUpload(ctx, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted upload returned %v, want ErrNotFound", err)
	}
}

func TestFSUploadSessionRejectsTraversal(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../outside", "/absolute", "oci/../outside", `oci\\outside`} {
		if err := store.CreateUpload(context.Background(), key); err == nil {
			t.Fatalf("unsafe upload key %q was accepted", key)
		}
	}
}

func digestForTest(content string) string {
	hash := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(hash[:])
}
