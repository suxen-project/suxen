package store

import (
	"context"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Consumer capability ports: narrow slices of the backend a specific consumer
// needs, so it holds a small interface rather than the whole Store. They live in
// this package — not beside each consumer, as narrow ports usually would — because
// they are shared across packages that cannot import each other (content hands
// them to oci, and the two form an import cycle). One SQLStore satisfies each.

// UploadLedger is the upload-session lifecycle capability: reserve or create a
// session, read it, commit an append, release an operation lease, delete a
// session, and sweep stale sessions.
type UploadLedger interface {
	ReserveUploadSession(context.Context, UploadSessionIdentity, string, time.Time, time.Time, int64, bool, int64, UploadSessionLimits) (UploadSessionReservation, error)
	ReserveUploadSessionCleanup(context.Context, UploadSessionIdentity, string, time.Time, time.Time) (UploadSessionReservation, error)
	CreateUploadSession(context.Context, UploadSession, UploadSessionLimits) error
	UploadSession(context.Context, string) (UploadSession, error)
	CommitUploadSessionAppend(context.Context, string, string, int64, time.Time, bool) error
	ReconcileUploadSessionSize(context.Context, UploadSessionIdentity, string, int64, time.Time) error
	ReleaseUploadSessionOperation(context.Context, string, string) error
	RenewUploadSessionOperation(context.Context, string, string, time.Time, time.Time) error
	ReleaseUploadSessionOperationUncertain(context.Context, string, string) error
	DeleteUploadSession(context.Context, string, string) error
	StaleUploadSessions(context.Context, string, time.Time, time.Time) ([]UploadSession, error)
}

// MetricsMetadata is the reporting capability the metrics collectors need: the
// aggregate row/resource counts and the per-store storage usage. Pool statistics
// are optional and taken through a DatabasePoolStatsProvider assertion on the same
// value, so a concrete backend that exposes them still works behind this port.
type MetricsMetadata interface {
	Stats(context.Context) (Stats, error)
	StorageUsage(context.Context) (StorageUsage, error)
}

// OCIMetadata is the repository, blob-store, and publication capability the OCI
// handler needs: resolve repositories and blob stores, publish assets, and prune
// dangling manifest aliases. Repository-scoped (ID-bound) access is a separate
// RepositoryView the runtime builds from its current backend, so a SetMetadata
// wrapper's overrides are honored.
type OCIMetadata interface {
	Repository(context.Context, string) (domain.Repository, error)
	BlobStore(context.Context, string) (domain.BlobStore, error)
	BlobStores(context.Context) ([]domain.BlobStore, error)
	WriteBlobStore(context.Context, string) (string, error)
	PutAsset(context.Context, domain.Asset) (domain.Asset, error)
	DeleteDanglingManifestAliases(context.Context, string, []domain.Asset) (int64, error)
}
