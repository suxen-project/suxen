package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Store owns strongly consistent repository metadata and authentication state.
// Implementations must preserve read-after-write semantics for mutating methods.
type Store interface {
	// Migrate applies all database migrations required by this binary.
	Migrate(context.Context) error
	// Ready verifies connectivity to the metadata database.
	Ready(context.Context) error
	// Close releases database resources.
	Close() error
	// SaveRepository commits a repository and its ownership record in one transaction.
	SaveRepository(context.Context, RepositorySave) error
	// DeleteRepository deletes a repository, its asset metadata, and its ownership
	// record in one transaction.
	DeleteRepository(context.Context, string, Ownership) error
	// Repository returns one repository by name.
	Repository(context.Context, string) (domain.Repository, error)
	// Repositories returns all repositories ordered by name.
	Repositories(context.Context) ([]domain.Repository, error)
	// RepositoriesPage reads at most limit repositories after a name.
	RepositoriesPage(context.Context, string, int) (RepositoryKeysetPage, error)
	// ForRepository mints an ID-bound view of one already-resolved repository.
	// Repository-scoped asset, gate, policy, cache, and attribute access goes
	// through this view rather than ID-keyed methods on the store, so the scope
	// travels with the resolved entity. A store wrapper that must observe those
	// scoped reads overrides ForRepository to return a view over itself.
	ForRepository(domain.Repository) RepositoryView
	// CreateBlobStore inserts a named blob storage backend.
	CreateBlobStore(context.Context, domain.BlobStore) error
	// SaveBlobStore commits a blob store and its ownership record in one transaction.
	SaveBlobStore(context.Context, BlobStoreSave) error
	// DeleteBlobStore deletes an unreferenced blob storage backend and its
	// ownership record in one transaction.
	DeleteBlobStore(context.Context, string, Ownership) error
	// SetBlobStoreState persists a blob store's lifecycle state and drain target.
	SetBlobStoreState(ctx context.Context, name string, state string, drainTarget string) error
	// BeginBlobStoreDrain atomically marks source draining onto target, locking
	// the target row and revalidating it so a concurrent target deletion cannot
	// leave a store draining onto a store that no longer exists.
	BeginBlobStoreDrain(ctx context.Context, source string, target string) error
	// RebindRepositories moves every repository bound to one blob store onto another.
	RebindRepositories(ctx context.Context, from string, to string) (int64, error)
	// BlobStore returns one named blob storage backend without resolved credentials.
	BlobStore(context.Context, string) (domain.BlobStore, error)
	// BlobStores returns all named blob storage backends ordered by name.
	BlobStores(context.Context) ([]domain.BlobStore, error)
	// WriteBlobStore resolves the store new bytes for the named store must be
	// written to: the store itself, or its drain target when it is draining, so a
	// draining store takes no new blobs.
	WriteBlobStore(ctx context.Context, blobStore string) (string, error)
	// CreateUploadSession records a new transient upload after atomically enforcing
	// its blob-store-scoped principal session-count limit.
	CreateUploadSession(context.Context, UploadSession, UploadSessionLimits) error
	// UploadSession returns one transient upload-session record.
	UploadSession(context.Context, string) (UploadSession, error)
	// CountUploadSessions reports all unfinished sessions staged in one store.
	CountUploadSessions(context.Context, string) (int64, error)
	// StaleUploadSessions returns one blob store's sessions whose last activity predates
	// staleBefore and that hold no operation lease still live at now, oldest first.
	StaleUploadSessions(context.Context, string, time.Time, time.Time) ([]UploadSession, error)
	// ReserveUploadSession atomically leases a session operation and reserves capacity.
	// When exactGrowth is false, the returned reservation may be smaller than maxGrowth.
	ReserveUploadSession(
		context.Context,
		UploadSessionIdentity,
		string,
		time.Time,
		time.Time,
		int64,
		bool,
		int64,
		UploadSessionLimits,
	) (UploadSessionReservation, error)
	// ReserveUploadSessionCleanup leases an existing session for cancellation or
	// stale cleanup without applying limits to bytes already staged.
	ReserveUploadSessionCleanup(context.Context, UploadSessionIdentity, string, time.Time, time.Time) (UploadSessionReservation, error)
	// CommitUploadSessionAppend records the stored size. retainOperation keeps the
	// exclusive lease for a completion request that must promote the staged upload.
	CommitUploadSessionAppend(context.Context, string, string, int64, time.Time, bool) error
	// ReconcileUploadSessionSize records an already-staged physical size under a
	// cleanup-style operation lease, without admitting any new growth.
	ReconcileUploadSessionSize(context.Context, UploadSessionIdentity, string, int64, time.Time) error
	// ReleaseUploadSessionOperation releases a failed or completed operation lease.
	ReleaseUploadSessionOperation(context.Context, string, string) error
	// RenewUploadSessionOperation extends a live lease only for its current owner.
	RenewUploadSessionOperation(context.Context, string, string, time.Time, time.Time) error
	// ReleaseUploadSessionOperationUncertain releases only the lease; it retains
	// reserved capacity until the staged object's size can be reconciled.
	ReleaseUploadSessionOperationUncertain(context.Context, string, string) error
	// DeleteUploadSession removes a session. A non-empty operation ID must own its lease;
	// an empty operation ID is reserved for compensating a failed session creation.
	DeleteUploadSession(context.Context, string, string) error
	// ProvisionRecord returns reconciliation metadata for one declaratively managed resource.
	ProvisionRecord(context.Context, string, string) (ProvisionRecord, error)
	// ProvisionRecords returns every declaratively managed resource ordered by kind and name.
	ProvisionRecords(context.Context) ([]ProvisionRecord, error)
	// PutProvisionRecord creates or replaces reconciliation metadata without storing secrets.
	PutProvisionRecord(context.Context, ProvisionRecord) error
	// DeleteProvisionRecord forgets declarative ownership of one resource.
	DeleteProvisionRecord(context.Context, string, string) error
	// PutAsset creates or replaces an asset at a repository-relative path.
	PutAsset(context.Context, domain.Asset) (domain.Asset, error)
	// PutAssets creates or replaces a set of assets in one metadata transaction.
	// Either every path becomes visible or none does.
	PutAssets(context.Context, []domain.Asset) ([]domain.Asset, error)
	// Asset returns one asset by repository and path.
	Asset(context.Context, string, string) (domain.Asset, error)
	// AssetByRepositoryID resolves an asset by repository ID and path for a caller
	// that holds only a member's ID (group shadow checks) and has no resolved
	// repository to mint a view from. It is identity-bound: same-name recreation
	// must never redirect it to replacement state. Every other repository-scoped
	// read and mutation goes through a RepositoryView minted by ForRepository.
	AssetByRepositoryID(context.Context, string, string) (domain.Asset, error)
	// AssetByPublicPath finds a stored asset at a client-facing path within one
	// repository identity and an optional asset-ID watermark. Group visibility
	// uses it only to test whether a prior member occupies that path.
	AssetByPublicPath(context.Context, string, string, int64) (domain.Asset, error)
	// AssetByID returns one asset by repository and stable numeric identifier.
	AssetByID(context.Context, string, int64) (domain.Asset, error)
	// DeleteAsset removes one asset mapping without deleting the underlying blob.
	DeleteAsset(context.Context, string, string) (domain.Asset, error)
	// DeleteOCIManifestByDigest atomically removes a digest-addressed OCI manifest
	// and every tag for that digest in the same image namespace.
	DeleteOCIManifestByDigest(context.Context, string, string) ([]domain.Asset, error)
	// DeleteAssetWithCompanions removes an artifact together with the
	// companion metadata records at the given repository-relative paths,
	// atomically. The artifact is deleted only if its stored row still matches
	// every field; companions are deleted by path restricted to the metadata
	// kind, whether or not they were observed, so a companion created
	// concurrently before deletion is not orphaned and a non-metadata asset
	// sharing a declared path is never removed. It reports false without
	// deleting anything when the artifact row has changed.
	DeleteAssetWithCompanions(context.Context, domain.Asset, []string) (bool, error)
	// DeleteAssetsIfUnchanged atomically removes all supplied asset rows only
	// while every field of every row still matches the supplied snapshot.
	DeleteAssetsIfUnchanged(context.Context, []domain.Asset) (bool, error)
	// DeleteAssetsInDirectoryIfUnchanged also requires the complete set of
	// direct children in directory to equal the supplied snapshot. It shares
	// the publication lock so a new file cannot appear between check and commit.
	DeleteAssetsInDirectoryIfUnchanged(context.Context, string, []domain.Asset) (bool, error)
	// Assets returns assets whose paths begin with prefix.
	Assets(context.Context, string, string) ([]domain.Asset, error)
	// MaxAssetID fixes a high-water mark across repository discovery cursors.
	MaxAssetID(context.Context) (int64, error)
	// ReferencedDigests returns blobs reachable within one named blob store.
	ReferencedDigests(context.Context, string) (map[string]struct{}, error)
	// PublishedReferencedDigests excludes unresolved OCI dependencies whose
	// bytes have never been published; verification uses it to identify blobs
	// expected to exist physically in this store.
	PublishedReferencedDigests(context.Context, string) (map[string]struct{}, error)
	// DeleteUnreferencedBlobAssets removes unreferenced OCI blob metadata from one blob store.
	DeleteUnreferencedBlobAssets(context.Context, string, string) (int64, error)
	// DeleteDanglingManifestAliases prunes canonical aliases released by removed tags.
	DeleteDanglingManifestAliases(context.Context, string, []domain.Asset) (int64, error)
	// NegativeCacheHit reports whether a repository path has an unexpired not-found entry.
	NegativeCacheHit(context.Context, string, string, time.Time) (bool, error)
	// ClearNegativeCache removes a cached not-found response.
	ClearNegativeCache(context.Context, string, string) error
	// TouchAsset records a successful download without changing artifact content.
	TouchAsset(context.Context, int64, time.Time) error
	// RefreshAsset records successful upstream validation without changing download activity.
	RefreshAsset(context.Context, int64, time.Time) error
	// SetAttributes replaces one namespaced attribute value on an asset. This is a
	// trusted storage primitive; external adapters must reject reserved namespaces.
	SetAttributes(context.Context, string, int64, string, map[string]any) error
	// DeleteAttributes removes one namespaced attribute value from an asset. This is a
	// trusted storage primitive; external adapters must reject reserved namespaces.
	DeleteAttributes(context.Context, string, int64, string) error
	// SaveUser commits an account, its role assignments, and its ownership
	// record in one transaction. Password empty keeps the existing hash on
	// update; a nil Roles slice keeps the existing assignments on update.
	SaveUser(context.Context, UserSave) error
	// CreateBootstrapAdmin creates the first administrator, role, and token in
	// one transaction. An existing username is left untouched.
	CreateBootstrapAdmin(context.Context, string, string, string) error
	// User returns one local account without credential material.
	User(context.Context, string) (domain.User, error)
	// Users returns local accounts without credential material.
	Users(context.Context) ([]domain.User, error)
	// ReplaceUserRoles replaces a user's direct roles and applies the ownership
	// effect in one transaction, leaving account fields untouched.
	ReplaceUserRoles(context.Context, string, []string, Ownership) error
	// DeleteUser removes a local account, role assignments, API tokens, and its
	// ownership record in one transaction.
	DeleteUser(context.Context, string, Ownership) error
	// AuthenticatePassword validates local username and password credentials.
	AuthenticatePassword(context.Context, string, string) (domain.User, bool)
	// CreateToken adds a hashed API token for a local account and returns its
	// public metadata. The clear-text token is never retained in the store.
	CreateToken(context.Context, string, string, string, []string) (domain.APIToken, error)
	// AuthenticateToken validates a bearer token.
	AuthenticateToken(context.Context, string) (domain.User, bool)
	// Tokens returns token metadata for a user without token secrets or hashes.
	Tokens(context.Context, string) ([]domain.APIToken, error)
	// DeleteToken revokes one token owned by a user.
	DeleteToken(context.Context, string, int64) error
	// SaveRole commits a role and its ownership record in one transaction.
	SaveRole(context.Context, RoleSave) error
	// DeleteRole removes a role, its assignments, and its ownership record in one
	// transaction; a role referenced by an OIDC provider mapping is rejected.
	DeleteRole(context.Context, string, Ownership) error
	// Role returns one role by name.
	Role(context.Context, string) (domain.Role, error)
	// Roles returns every role ordered by name.
	Roles(context.Context) ([]domain.Role, error)
	// UserRoles returns roles directly assigned to a local user.
	UserRoles(context.Context, string) ([]string, error)
	// EffectivePrivileges resolves directly assigned roles for a subject name.
	EffectivePrivileges(context.Context, string) ([]string, error)
	// LocalAuthorization resolves a local account's current admin status, roles,
	// and privileges only while its immutable identity still matches.
	LocalAuthorization(context.Context, string, string) (domain.User, []string, error)
	// PrivilegesForRoles resolves roles supplied by an external identity.
	PrivilegesForRoles(context.Context, []string) ([]string, error)
	// SaveOIDCProvider commits a provider configuration and its ownership record
	// in one transaction.
	SaveOIDCProvider(context.Context, OIDCSave) error
	// DeleteOIDCProvider removes a provider and its ownership record in one
	// transaction.
	DeleteOIDCProvider(context.Context, string, Ownership) error
	// OIDCProvider returns one external identity provider by name.
	OIDCProvider(context.Context, string) (domain.OIDCProvider, error)
	// OIDCProviderByIssuer returns one external identity provider by issuer URL.
	OIDCProviderByIssuer(context.Context, string) (domain.OIDCProvider, error)
	// OIDCProviders returns all configured external identity providers.
	OIDCProviders(context.Context) ([]domain.OIDCProvider, error)
	// Classification returns the ordered classification rules for a repository.
	Classification(context.Context, string) (domain.ClassificationConfig, error)
	// SaveClassification commits a repository's rules, the asset relabel, and its
	// ownership record in one transaction.
	SaveClassification(context.Context, ClassificationSave) error
	// DeleteClassification removes stored rules, clears classification.* from assets,
	// and removes the ownership record, in one transaction.
	DeleteClassification(context.Context, string, Ownership) error
	// SaveClassificationDefaults commits the instance-wide default, the relabel of
	// inheriting repositories, and its ownership record in one transaction.
	SaveClassificationDefaults(context.Context, ClassificationDefaultsSave) error
	// ClassificationDefaults returns the instance-wide classification default.
	ClassificationDefaults(context.Context) (domain.ClassificationConfig, error)
	// DeleteClassificationDefaults removes the instance-wide default, relabels
	// inheriting repositories, and removes the ownership record, in one transaction.
	DeleteClassificationDefaults(context.Context, Ownership) error
	// SaveCleanupPolicy commits a cleanup policy and its ownership record in one
	// transaction.
	SaveCleanupPolicy(context.Context, CleanupPolicySave) error
	// DeleteCleanupPolicy removes a cleanup policy and its ownership record in one
	// transaction.
	DeleteCleanupPolicy(context.Context, string, Ownership) error
	// CleanupPolicy returns one cleanup policy by name.
	CleanupPolicy(context.Context, string) (domain.CleanupPolicy, error)
	// CleanupPolicies returns cleanup policies ordered by name.
	CleanupPolicies(context.Context) ([]domain.CleanupPolicy, error)
	// CreateTask records a queued administrative operation.
	CreateTask(context.Context, domain.Task) (domain.Task, error)
	// UpdateTask replaces the mutable state of an administrative operation.
	UpdateTask(context.Context, domain.Task) error
	// Task returns one administrative operation by identifier.
	Task(context.Context, int64) (domain.Task, error)
	// TaskPage returns a bounded reverse-ID page from a stable creation snapshot.
	TaskPage(context.Context, int64, int64, int) (IDPage[domain.Task], error)
	// AcquireLease atomically claims or renews a named scheduler lease.
	AcquireLease(context.Context, string, string, time.Time, time.Time) (bool, error)
	// Lease returns the current owner and expiry of a named scheduler lease.
	Lease(context.Context, string) (domain.Lease, error)
	// ReleaseLease removes a lease only when holder still owns it.
	ReleaseLease(context.Context, string, string) error
	// SaveWebhook commits a webhook subscription and its ownership record in one
	// transaction.
	SaveWebhook(context.Context, WebhookSave) error
	// DeleteWebhook removes a subscription and its ownership record in one
	// transaction, along with its delivery history.
	DeleteWebhook(context.Context, string, Ownership) error
	// Webhook returns one subscription by name; its secret remains excluded from JSON output.
	Webhook(context.Context, string) (domain.Webhook, error)
	// Webhooks returns all outbound subscriptions ordered by name.
	Webhooks(context.Context) ([]domain.Webhook, error)
	// EnqueueWebhookEvent creates deliveries for every matching enabled subscription.
	EnqueueWebhookEvent(context.Context, domain.WebhookEvent) error
	// ClaimWebhookDeliveries leases due deliveries to one cluster worker.
	ClaimWebhookDeliveries(
		context.Context,
		string,
		time.Time,
		time.Duration,
		int,
	) ([]domain.WebhookDelivery, error)
	// CompleteWebhookDelivery records a leased delivery's terminal or retry state.
	CompleteWebhookDelivery(
		context.Context,
		int64,
		string,
		string,
		time.Time,
		string,
	) error
	// WebhookDeliveryPage returns a bounded reverse-ID page from a stable creation snapshot.
	WebhookDeliveryPage(context.Context, string, int64, int64, int) (IDPage[domain.WebhookDelivery], error)
	// SaveDownloadGate commits a repository read gate and its ownership record in
	// one transaction.
	SaveDownloadGate(context.Context, DownloadGateSave) error
	// DownloadGate returns a repository's read gate.
	DownloadGate(context.Context, string) (domain.DownloadGate, error)
	// DeleteDownloadGate removes a repository's read gate and its ownership record.
	DeleteDownloadGate(context.Context, string, Ownership) error
	// SaveDownloadGateDefaults commits the instance-wide download-gate default and
	// its ownership record in one transaction.
	SaveDownloadGateDefaults(context.Context, DownloadGateDefaultsSave) error
	// DownloadGateDefaults returns the instance-wide download-gate default.
	DownloadGateDefaults(context.Context) (domain.DownloadGate, error)
	// DeleteDownloadGateDefaults removes the instance-wide download-gate default and its ownership record.
	DeleteDownloadGateDefaults(context.Context, Ownership) error
	// SaveTrustPolicy creates or replaces a repository signature policy and its
	// provisioning ownership record in one transaction. Groups enforce their
	// supplying member's policy and reject their own.
	SaveTrustPolicy(context.Context, TrustPolicySave) error
	// TrustPolicy returns a repository signature policy and public trust material.
	TrustPolicy(context.Context, string) (domain.TrustPolicy, error)
	// DeleteTrustPolicy removes a repository signature policy and its ownership record.
	DeleteTrustPolicy(context.Context, string, Ownership) error
	// EffectiveTrustPolicy returns a repository's own policy, else the instance-wide default.
	EffectiveTrustPolicy(context.Context, string) (domain.TrustPolicy, error)
	// SaveTrustPolicyDefaults creates or replaces the instance-wide trust-policy
	// default and its ownership record in one transaction.
	SaveTrustPolicyDefaults(context.Context, TrustPolicyDefaultsSave) error
	// TrustPolicyDefaults returns the instance-wide trust-policy default.
	TrustPolicyDefaults(context.Context) (domain.TrustPolicy, error)
	// DeleteTrustPolicyDefaults removes the instance-wide trust-policy default and its ownership record.
	DeleteTrustPolicyDefaults(context.Context, Ownership) error
	// Stats returns current storage and repository counters.
	Stats(context.Context) (Stats, error)
	StorageUsage(context.Context) (StorageUsage, error)
}

// DatabasePoolStatsProvider exposes an in-memory SQL connection-pool snapshot for
// observability. Implementations must not execute a database query from this method.
type DatabasePoolStatsProvider interface {
	// DatabasePoolStats returns a non-blocking snapshot of the current SQL pool.
	DatabasePoolStats() DatabasePoolStats
}

// DatabasePoolStats identifies a metadata backend and its current database/sql pool
// state. Backend is a bounded implementation name such as "sqlite" or "postgres".
type DatabasePoolStats struct {
	Backend string
	Stats   sql.DBStats
}

// UploadSessionIdentity contains the immutable request coordinates used to prevent
// one principal, repository, or image from resuming another upload.
type UploadSessionIdentity struct {
	ID         string
	Repository string
	Image      string
	BlobStore  string
	Principal  string
}

// UploadSession is the database-authoritative record for one mutable blob upload.
type UploadSession struct {
	UploadSessionIdentity
	// RepositoryID pins creation to the repository resolved by the request.
	RepositoryID     string
	StorageKey       string
	Size             int64
	ReservedBytes    int64
	OperationID      string
	OperationExpires time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// UploadSessionLimits are aggregate admission limits for one blob store.
type UploadSessionLimits struct {
	MaxStagedBytes          int64
	MaxPrincipalStagedBytes int64
	MaxPrincipalSessions    int64
}

// UploadSessionReservation describes the exclusive capacity granted to an operation.
type UploadSessionReservation struct {
	Session      UploadSession
	ReservedGrow int64
	MaxSize      int64
}

// ProvisionRecord tracks declarative ownership and non-reversible comparison
// material. SecretFingerprint contains a salted digest, never the secret value.
type ProvisionRecord struct {
	Kind              string
	Name              string
	SecretFingerprint string
	UpdatedAt         time.Time
}

// IDPage is a bounded page ordered by descending immutable row ID. SnapshotID
// fixes the newest visible row for every continuation in the traversal.
type IDPage[T any] struct {
	Items      []T
	SnapshotID int64
	HasMore    bool
}

// Stats summarizes persisted repository metadata for metrics and administration.
type Stats struct {
	Repositories int64 `json:"repositories"`
	Assets       int64 `json:"assets"`
	UniqueBlobs  int64 `json:"uniqueBlobs"`
	Bytes        int64 `json:"bytes"`
	WebhookQueue int64 `json:"webhookQueue"`
	WebhookDead  int64 `json:"webhookDead"`
}

// StorageUsage breaks referenced blob bytes down by repository and by blob store,
// each deduplicated by digest within its group. Repository totals are a logical
// footprint — a blob shared by two repositories counts toward both — while
// blob-store totals are the physical footprint of the store that holds the bytes.
type StorageUsage struct {
	Repositories []RepositoryUsage `json:"repositories"`
	BlobStores   []BlobStoreUsage  `json:"blobStores"`
}

// RepositoryUsage is one repository's deduplicated blob footprint.
type RepositoryUsage struct {
	Repository string `json:"repository"`
	Blobs      int64  `json:"blobs"`
	Bytes      int64  `json:"bytes"`
}

// BlobStoreUsage is one blob store's deduplicated physical footprint.
type BlobStoreUsage struct {
	BlobStore string `json:"blobStore"`
	Blobs     int64  `json:"blobs"`
	Bytes     int64  `json:"bytes"`
}

// AssetPageRequest selects a path-keyset page. MaxID freezes the set of
// eligible asset identities for cursor traversal.
type AssetPageRequest struct {
	Prefix  string
	AfterID int64
	MaxID   int64
	Limit   int
	// PublicPrefix matches FormatPath when present, otherwise Path. The
	// cursor still walks stored paths and IDs.
	PublicPrefix bool
}

// AssetPage is bounded to Limit items.
type AssetPage struct {
	Items   []domain.Asset
	HasMore bool
	MaxID   int64
}

// RepositoryKeysetPage is bounded to the requested row count.
type RepositoryKeysetPage struct {
	Items   []domain.Repository
	HasMore bool
}
