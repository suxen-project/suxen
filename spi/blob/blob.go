// Package blob is the public service-provider interface for suxen blob
// storage drivers.
//
// A blob store persists immutable, content-addressed blobs. Digests are
// strings of the form "sha256:<hex>", already normalized and validated by the
// server before they reach a driver.
//
// Drivers are compiled into the binary and announce themselves with Register,
// typically from an init function in the driver package. The server resolves
// drivers by name (for named blob-store resources) or by configuration URL
// scheme (for the process-level default store).
package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/suxen-project/suxen/spi/internal/ident"
)

// Info describes a stored blob.
type Info struct {
	Digest     string    `json:"digest"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

var (
	// ErrNotFound reports that a blob or upload session does not exist. Get,
	// Head and OpenUpload must return it (possibly wrapped) for missing keys.
	ErrNotFound = errors.New("not found")
	// ErrDigestMismatch reports that streamed content did not match the
	// requested digest. Put must verify content before making it visible.
	ErrDigestMismatch = errors.New("content digest does not match")
	// ErrUploadTooLarge indicates that appending data would exceed an upload's
	// size limit. AppendUpload must fail without changing the session.
	ErrUploadTooLarge = errors.New("upload too large")
	// ErrUploadExists reports that CreateUpload was called for a key that
	// already holds an upload session.
	ErrUploadExists = errors.New("upload already exists")
	// ErrUploadStoreUnsupported indicates that a driver cannot persist upload
	// sessions. Drivers without UploadStore support leave staged uploads to the
	// server's local spool.
	ErrUploadStoreUnsupported = errors.New("blob store does not support upload sessions")
)

// Store persists immutable blobs by digest.
//
// Contract, in addition to the per-method comments:
//   - Put must verify that the source bytes match the requested digest and
//     must make concurrent writes of the same digest idempotent. It may spool
//     the stream to local disk before uploading.
//   - Get, Head and Delete receive normalized digests; Get and Head return
//     ErrNotFound for absent blobs, Delete of an absent blob is a no-op.
//   - Walk yields each blob without materializing the entire store. It must
//     stop on callback error and return that error, including cancellation.
//   - Streaming methods must honor context cancellation.
//   - A driver may implement io.Closer; the server closes a store when its
//     configuration is replaced and no operation is in flight.
type Store interface {
	// Put streams and persists a blob after verifying its digest.
	Put(ctx context.Context, digest string, source io.Reader) (Info, error)
	// Get opens a blob for streaming.
	Get(ctx context.Context, digest string) (io.ReadCloser, Info, error)
	// Head returns blob metadata without retaining an open reader.
	Head(ctx context.Context, digest string) (Info, error)
	// Walk yields each blob available in this store. Callbacks may read and
	// delete blobs, and their errors stop enumeration immediately.
	Walk(ctx context.Context, yield func(Info) error) error
	// Delete removes a blob during garbage collection.
	Delete(ctx context.Context, digest string) error
	// Ready verifies that the backing store is writable and reachable.
	Ready(ctx context.Context) error
}

// UploadStore persists mutable, transient upload sessions alongside immutable
// blobs. Implementing it is optional; it lets chunked client uploads (such as
// OCI upload sessions) survive replica failover.
//
// Upload keys are opaque, slash-separated identifiers supplied by the server.
// Implementations must keep them outside the immutable content-addressed
// namespace.
type UploadStore interface {
	// CreateUpload creates an empty upload and fails with ErrUploadExists if
	// the key already exists.
	CreateUpload(ctx context.Context, key string) error
	// AppendUpload appends all source bytes and returns the resulting size.
	// The append must either persist completely or leave the previous session
	// content unchanged, and must fail with ErrUploadTooLarge when maxSize
	// would be exceeded.
	AppendUpload(ctx context.Context, key string, source io.Reader, maxSize int64) (int64, error)
	// OpenUpload opens an upload for streaming and returns its current size.
	OpenUpload(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// DeleteUpload removes an upload. Missing uploads return ErrNotFound.
	DeleteUpload(ctx context.Context, key string) error
}

// Driver describes a compiled-in blob storage backend.
type Driver struct {
	// Name identifies the driver in blob-store resources ("driver": "gcs").
	// It must be a non-empty lowercase identifier (letter, then letters,
	// digits, or hyphens) and unique per process.
	Name string

	// Open constructs a store from its resolved configuration string. The
	// configuration is driver-defined; URL-shaped configurations should use
	// the scheme claimed in URLScheme (for example "gcs://bucket/prefix").
	// Open must validate the configuration and may resolve local credential
	// sources, but must not probe the backing storage over the network;
	// readiness is probed separately through Store.Ready.
	Open func(configuration string) (Store, error)

	// CanonicalConfiguration normalizes a configuration string so that two
	// configurations addressing the same physical storage compare equal. The
	// server hashes the canonical form to detect when two blob-store
	// resources collide on the same backing storage and when a resource's
	// configuration changes. When nil, the trimmed configuration string is
	// used verbatim, which misses aliases such as equivalent paths or
	// default ports.
	CanonicalConfiguration func(configuration string) (string, error)

	// URLScheme optionally claims a URL scheme so the driver can serve as the
	// process-level default blob store (SUXEN_BLOBSTORE=<scheme>://...). Empty
	// means the driver is only available to named blob-store resources.
	// A non-empty scheme starts with a lowercase letter and then contains
	// lowercase letters, digits, '+', '-', or '.'.
	URLScheme string

	// SharedStorage reports that the backing storage is remote and reachable
	// by every replica. It gates multi-replica (cluster) deployments and
	// enables strict bootstrap-credential validation; local-disk drivers must
	// leave it false.
	SharedStorage bool
}

// validate reports why a driver registration is malformed.
func (d Driver) validate() error {
	if d.Name == "" {
		return errors.New("blob driver name is required")
	}
	if !ident.Valid(d.Name) {
		return errors.New("blob driver name must be a lowercase identifier")
	}
	if d.URLScheme != "" && !validURLScheme(d.URLScheme) {
		return errors.New("blob driver URL scheme must use lowercase RFC 3986 scheme characters")
	}
	if d.Open == nil {
		return errors.New("blob driver Open is required")
	}
	return nil
}

func validURLScheme(scheme string) bool {
	for index, r := range scheme {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if index > 0 && ((r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.') {
			continue
		}
		return false
	}
	return scheme != ""
}
