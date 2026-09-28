// Package blobtest provides the conformance suite for blob storage drivers.
//
// Driver authors run the suite from a regular Go test:
//
//	func TestConformance(t *testing.T) {
//		blobtest.RunStoreSuite(t, func(t *testing.T) blob.Store {
//			return newTestStore(t)
//		})
//	}
//
// The factory is called once per subtest and must return an empty store.
// Cleanup belongs to the factory (use t.Cleanup). When the returned store
// also implements blob.UploadStore, the upload-session contract is exercised
// as well.
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/blob"
)

// Factory returns an empty store for one subtest.
type Factory func(t *testing.T) blob.Store

// RunStoreSuite exercises the blob.Store contract, and the blob.UploadStore
// contract when the store implements it.
func RunStoreSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("PutGetRoundtrip", func(t *testing.T) {
		store := factory(t)
		content := []byte("conformance blob content")
		digest := digestOf(content)

		info, err := store.Put(context.Background(), digest, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if info.Digest != digest {
			t.Fatalf("Put digest = %q, want %q", info.Digest, digest)
		}
		if info.Size != int64(len(content)) {
			t.Fatalf("Put size = %d, want %d", info.Size, len(content))
		}

		reader, getInfo, err := store.Get(context.Background(), digest)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer reader.Close()
		read, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if !bytes.Equal(read, content) {
			t.Fatalf("Get content = %q, want %q", read, content)
		}
		if getInfo.Digest != digest || getInfo.Size != int64(len(content)) {
			t.Fatalf("Get info = %+v", getInfo)
		}

		headInfo, err := store.Head(context.Background(), digest)
		if err != nil {
			t.Fatalf("Head: %v", err)
		}
		if headInfo.Digest != digest || headInfo.Size != int64(len(content)) {
			t.Fatalf("Head info = %+v", headInfo)
		}
	})

	t.Run("PutVerifiesDigest", func(t *testing.T) {
		store := factory(t)
		wrong := digestOf([]byte("other content"))
		_, err := store.Put(context.Background(), wrong, strings.NewReader("actual content"))
		if !errors.Is(err, blob.ErrDigestMismatch) {
			t.Fatalf("Put with wrong digest error = %v, want ErrDigestMismatch", err)
		}
		if _, err := store.Head(context.Background(), wrong); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("mismatched blob must not be visible, Head error = %v", err)
		}
	})

	t.Run("PutIsIdempotent", func(t *testing.T) {
		store := factory(t)
		content := []byte("stored twice")
		digest := digestOf(content)
		if _, err := store.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
			t.Fatalf("first Put: %v", err)
		}
		info, err := store.Put(context.Background(), digest, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("second Put: %v", err)
		}
		if info.Size != int64(len(content)) {
			t.Fatalf("second Put size = %d, want %d", info.Size, len(content))
		}
	})

	t.Run("MissingBlobs", func(t *testing.T) {
		store := factory(t)
		missing := digestOf([]byte("never stored"))
		if _, _, err := store.Get(context.Background(), missing); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Get missing error = %v, want ErrNotFound", err)
		}
		if _, err := store.Head(context.Background(), missing); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Head missing error = %v, want ErrNotFound", err)
		}
		if err := store.Delete(context.Background(), missing); err != nil {
			t.Fatalf("Delete of a missing blob must be a no-op, got %v", err)
		}
	})

	t.Run("DeleteRemovesBlob", func(t *testing.T) {
		store := factory(t)
		content := []byte("delete me")
		digest := digestOf(content)
		if _, err := store.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete(context.Background(), digest); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.Head(context.Background(), digest); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Head after delete error = %v, want ErrNotFound", err)
		}
	})

	t.Run("WalkYieldsAllBlobs", func(t *testing.T) {
		store := factory(t)
		contents := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
		want := make(map[string]int64, len(contents))
		for _, content := range contents {
			digest := digestOf(content)
			want[digest] = int64(len(content))
			if _, err := store.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		found := make(map[string]int64)
		err := store.Walk(context.Background(), func(info blob.Info) error {
			found[info.Digest] = info.Size
			return nil
		})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		for digest, size := range want {
			if found[digest] != size {
				t.Fatalf("Walk missing %s (size %d): got %v", digest, size, found)
			}
		}
	})

	t.Run("WalkStopsOnCallbackError", func(t *testing.T) {
		store := factory(t)
		content := []byte("walk stop")
		if _, err := store.Put(context.Background(), digestOf(content), bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		stop := errors.New("stop walk")
		calls := 0
		err := store.Walk(context.Background(), func(blob.Info) error {
			calls++
			return stop
		})
		if !errors.Is(err, stop) || calls != 1 {
			t.Fatalf("Walk error = %v, callback calls = %d", err, calls)
		}
	})

	t.Run("WalkHonorsCancellation", func(t *testing.T) {
		store := factory(t)
		content := []byte("walk cancel")
		if _, err := store.Put(context.Background(), digestOf(content), bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := store.Walk(ctx, func(blob.Info) error {
			calls++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("Walk error = %v, callback calls = %d", err, calls)
		}
	})

	t.Run("WalkCallbackMayReadAndDelete", func(t *testing.T) {
		store := factory(t)
		content := []byte("walk and delete")
		digest := digestOf(content)
		if _, err := store.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		err := store.Walk(context.Background(), func(info blob.Info) error {
			if _, err := store.Head(context.Background(), info.Digest); err != nil {
				return err
			}
			return store.Delete(context.Background(), info.Digest)
		})
		if err != nil {
			t.Fatalf("Walk with read/delete callback: %v", err)
		}
		if _, err := store.Head(context.Background(), digest); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("deleted blob remains: %v", err)
		}
	})

	t.Run("Ready", func(t *testing.T) {
		store := factory(t)
		if err := store.Ready(context.Background()); err != nil {
			t.Fatalf("Ready: %v", err)
		}
	})

	t.Run("UploadSessions", func(t *testing.T) {
		store := factory(t)
		uploads, supported := store.(blob.UploadStore)
		if !supported {
			t.Skip("store does not implement blob.UploadStore")
		}
		runUploadSuite(t, uploads)
	})
}

func runUploadSuite(t *testing.T, uploads blob.UploadStore) {
	t.Helper()
	const key = "conformance/upload-session"

	if err := uploads.CreateUpload(context.Background(), key); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if err := uploads.CreateUpload(context.Background(), key); !errors.Is(err, blob.ErrUploadExists) {
		t.Fatalf("duplicate CreateUpload error = %v, want ErrUploadExists", err)
	}

	size, err := uploads.AppendUpload(context.Background(), key, strings.NewReader("first "), 1<<20)
	if err != nil {
		t.Fatalf("AppendUpload: %v", err)
	}
	if size != int64(len("first ")) {
		t.Fatalf("AppendUpload size = %d, want %d", size, len("first "))
	}
	size, err = uploads.AppendUpload(context.Background(), key, strings.NewReader("second"), 1<<20)
	if err != nil {
		t.Fatalf("second AppendUpload: %v", err)
	}
	if size != int64(len("first second")) {
		t.Fatalf("second AppendUpload size = %d, want %d", size, len("first second"))
	}

	if _, err := uploads.AppendUpload(
		context.Background(),
		key,
		strings.NewReader("overflow"),
		size+2,
	); !errors.Is(err, blob.ErrUploadTooLarge) {
		t.Fatalf("oversized AppendUpload error = %v, want ErrUploadTooLarge", err)
	}
	readFailure := errors.New("source read failed")
	for _, sameRead := range []bool{false, true} {
		if _, err := uploads.AppendUpload(
			context.Background(),
			key,
			&errorAfterDataReader{reader: strings.NewReader("partial"), err: readFailure, sameRead: sameRead},
			1<<20,
		); !errors.Is(err, readFailure) {
			t.Fatalf("source-error AppendUpload (same read: %t) error = %v, want source error", sameRead, err)
		}
	}

	reader, openSize, err := uploads.OpenUpload(context.Background(), key)
	if err != nil {
		t.Fatalf("OpenUpload: %v", err)
	}
	content, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		t.Fatalf("read upload: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close upload reader: %v", closeErr)
	}
	if string(content) != "first second" {
		t.Fatalf("upload content = %q, want %q (failed append must not change the session)", content, "first second")
	}
	if openSize != int64(len("first second")) {
		t.Fatalf("OpenUpload size = %d, want %d", openSize, len("first second"))
	}
	if size, err := uploads.AppendUpload(context.Background(), key, strings.NewReader("!"), 1<<20); err != nil || size != int64(len("first second!")) {
		t.Fatalf("AppendUpload after source error: size = %d, error = %v", size, err)
	}

	if err := uploads.DeleteUpload(context.Background(), key); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	if _, _, err := uploads.OpenUpload(context.Background(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("OpenUpload after delete error = %v, want ErrNotFound", err)
	}
	if err := uploads.DeleteUpload(context.Background(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("DeleteUpload of missing session error = %v, want ErrNotFound", err)
	}

	const maxKey = "conformance/max-size-upload-session"
	if err := uploads.CreateUpload(context.Background(), maxKey); err != nil {
		t.Fatalf("CreateUpload with maximum size: %v", err)
	}
	if size, err := uploads.AppendUpload(context.Background(), maxKey, strings.NewReader("x"), math.MaxInt64); err != nil || size != 1 {
		t.Fatalf("AppendUpload with maximum size: size = %d, error = %v", size, err)
	}
	maxReader, maxSize, err := uploads.OpenUpload(context.Background(), maxKey)
	if err != nil {
		t.Fatalf("OpenUpload with maximum size: %v", err)
	}
	maxContent, readErr := io.ReadAll(maxReader)
	maxCloseErr := maxReader.Close()
	if readErr != nil || maxCloseErr != nil || maxSize != 1 || string(maxContent) != "x" {
		t.Fatalf("maximum-size upload: size = %d, content = %q, read error = %v, close error = %v", maxSize, maxContent, readErr, maxCloseErr)
	}
	if err := uploads.DeleteUpload(context.Background(), maxKey); err != nil {
		t.Fatalf("DeleteUpload with maximum size: %v", err)
	}
}

// errorAfterDataReader simulates a source that fails only after supplying bytes.
type errorAfterDataReader struct {
	reader   io.Reader
	err      error
	sameRead bool
}

func (r *errorAfterDataReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.sameRead {
		return n, r.err
	}
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
