// Package memblob is an example out-of-tree blob store plugin: an in-memory
// store, registered as the "mem" driver. It demonstrates the full driver
// surface — registration, canonical configuration, and upload sessions —
// against nothing but the public suxen SPI. Contents vanish on restart, so
// it is only good for demos and tests.
package memblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/suxen-project/suxen/spi/blob"
)

func init() {
	blob.Register(blob.Driver{
		Name:      "mem",
		URLScheme: "mem",
		Open: func(configuration string) (blob.Store, error) {
			if !strings.HasPrefix(configuration, "mem://") {
				return nil, errors.New("memory blob store configuration must use mem://")
			}
			return &Store{
				blobs:   make(map[string]entry),
				uploads: make(map[string][]byte),
			}, nil
		},
		CanonicalConfiguration: func(configuration string) (string, error) {
			if !strings.HasPrefix(configuration, "mem://") {
				return "", errors.New("memory blob store configuration must use mem://")
			}
			return strings.TrimRight(configuration, "/"), nil
		},
		// Memory is process-local: never safe for multi-replica deployments.
		SharedStorage: false,
	})
}

type entry struct {
	data     []byte
	modified time.Time
}

// Store keeps blobs and upload sessions in process memory.
type Store struct {
	mu      sync.RWMutex
	blobs   map[string]entry
	uploads map[string][]byte
}

// Put verifies the digest and stores the content.
func (store *Store) Put(
	ctx context.Context,
	digest string,
	source io.Reader,
) (blob.Info, error) {
	hash := sha256.New()
	data, err := io.ReadAll(io.TeeReader(source, hash))
	if err != nil {
		return blob.Info{}, err
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		return blob.Info{}, fmt.Errorf(
			"%w: expected %s, got %s", blob.ErrDigestMismatch, digest, actual,
		)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.blobs[digest]; !exists {
		store.blobs[digest] = entry{data: data, modified: time.Now().UTC()}
	}
	stored := store.blobs[digest]
	return blob.Info{Digest: digest, Size: int64(len(stored.data)), ModifiedAt: stored.modified}, nil
}

// Get opens a stored blob.
func (store *Store) Get(
	ctx context.Context,
	digest string,
) (io.ReadCloser, blob.Info, error) {
	info, err := store.Head(ctx, digest)
	if err != nil {
		return nil, blob.Info{}, err
	}
	store.mu.RLock()
	data := store.blobs[digest].data
	store.mu.RUnlock()
	return io.NopCloser(bytes.NewReader(data)), info, nil
}

// Head returns blob metadata.
func (store *Store) Head(_ context.Context, digest string) (blob.Info, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	stored, found := store.blobs[digest]
	if !found {
		return blob.Info{}, blob.ErrNotFound
	}
	return blob.Info{Digest: digest, Size: int64(len(stored.data)), ModifiedAt: stored.modified}, nil
}

// Walk yields every stored blob.
func (store *Store) Walk(ctx context.Context, yield func(blob.Info) error) error {
	store.mu.RLock()
	// Snapshot keys so the callback can read or delete without holding a lock.
	digests := make([]string, 0, len(store.blobs))
	for digest := range store.blobs {
		digests = append(digests, digest)
	}
	store.mu.RUnlock()
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		store.mu.RLock()
		stored, found := store.blobs[digest]
		store.mu.RUnlock()
		if !found {
			continue
		}
		if err := yield(blob.Info{
			Digest:     digest,
			Size:       int64(len(stored.data)),
			ModifiedAt: stored.modified,
		}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Delete removes a blob; deleting a missing blob is a no-op.
func (store *Store) Delete(_ context.Context, digest string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.blobs, digest)
	return nil
}

// Ready always succeeds for process memory.
func (store *Store) Ready(_ context.Context) error { return nil }

// CreateUpload starts an empty upload session.
func (store *Store) CreateUpload(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.uploads[key]; exists {
		return fmt.Errorf("%w: %s", blob.ErrUploadExists, key)
	}
	store.uploads[key] = nil
	return nil
}

// AppendUpload appends fully or leaves the session unchanged.
func (store *Store) AppendUpload(
	_ context.Context,
	key string,
	source io.Reader,
	maxSize int64,
) (int64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	session, found := store.uploads[key]
	if !found {
		return 0, blob.ErrNotFound
	}
	initialSize := int64(len(session))
	if initialSize > maxSize {
		return initialSize, blob.ErrUploadTooLarge
	}
	remaining := maxSize - initialSize
	readLimit := remaining
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	appended, err := io.ReadAll(io.LimitReader(source, readLimit))
	if err != nil {
		return initialSize, err
	}
	if int64(len(appended)) > remaining {
		return initialSize, blob.ErrUploadTooLarge
	}
	store.uploads[key] = append(session, appended...)
	return int64(len(store.uploads[key])), nil
}

// OpenUpload opens an upload session for streaming.
func (store *Store) OpenUpload(
	_ context.Context,
	key string,
) (io.ReadCloser, int64, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	session, found := store.uploads[key]
	if !found {
		return nil, 0, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(session)), int64(len(session)), nil
}

// DeleteUpload removes an upload session.
func (store *Store) DeleteUpload(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, found := store.uploads[key]; !found {
		return blob.ErrNotFound
	}
	delete(store.uploads, key)
	return nil
}

var _ blob.Store = (*Store)(nil)
var _ blob.UploadStore = (*Store)(nil)
