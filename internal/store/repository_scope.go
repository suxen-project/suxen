package store

import (
	"context"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// RepositoryView is bound to one immutable repository ID. It only exposes
// operations used by requests that have already resolved a repository. Global
// defaults, repository administration, and other metadata remain on Store.
type RepositoryView interface {
	Repository(context.Context) (domain.Repository, error)
	DownloadGate(context.Context) (domain.DownloadGate, error)
	EffectiveTrustPolicy(context.Context) (domain.TrustPolicy, error)
	Asset(context.Context, string) (domain.Asset, error)
	AssetByID(context.Context, int64) (domain.Asset, error)
	Assets(context.Context, string) ([]domain.Asset, error)
	AssetPaths(context.Context, string, string, int) ([]string, error)
	AssetPage(context.Context, AssetPageRequest) (AssetPage, error)
	// ComponentVersionPage pages distinct stored component versions after
	// the given one (nil starts at the beginning) and returns every asset of
	// the versions on the page.
	ComponentVersionPage(context.Context, *ComponentVersion, int) (ComponentVersionPage, error)
	// AssetCount counts the repository's asset rows.
	AssetCount(context.Context) (int, error)
	// RetentionGroupPage pages distinct stored retention groups after the
	// given one; RetentionGroupAssets reads one group's rows.
	RetentionGroupPage(context.Context, string, int) ([]string, bool, error)
	RetentionGroupAssets(context.Context, string) ([]domain.Asset, error)
	// DirectoryAssets reads a directory's direct children ("" is the root).
	DirectoryAssets(context.Context, string) ([]domain.Asset, error)
	// SubtreeAssets reads every asset below a directory.
	SubtreeAssets(context.Context, string) ([]domain.Asset, error)
	DeleteAsset(context.Context, string) (domain.Asset, error)
	DeleteAssetWithCompanions(context.Context, string, []string) (domain.Asset, error)
	DeleteAssetByIDWithCompanions(context.Context, int64, []string) (domain.Asset, error)
	DeleteOCIManifestByDigest(context.Context, string) ([]domain.Asset, error)
	NegativeCacheHit(context.Context, string, time.Time) (bool, error)
	BeginProxyFetch(context.Context, string, time.Time) (domain.ProxyFetchToken, error)
	PublishProxyNotFound(context.Context, string, domain.ProxyFetchToken, time.Time) (bool, error)
	PublishProxyNotModified(context.Context, string, int64, domain.ProxyFetchToken, time.Time) (bool, error)
	PutNegativeCache(context.Context, string, time.Time) error
	ClearNegativeCache(context.Context, string) error
	TouchAsset(context.Context, int64, time.Time) error
	RefreshAsset(context.Context, int64, time.Time) error
	SetAttributes(context.Context, int64, string, map[string]any) error
	DeleteAttributes(context.Context, int64, string) error
}

// repositoryBackend is the small set of ID-based primitives backing a view.
// Callers receive RepositoryView, never this backend or a full Store.
type repositoryBackend interface {
	RepositoryByID(context.Context, string) (domain.Repository, error)
	DownloadGateByRepositoryID(context.Context, string) (domain.DownloadGate, error)
	EffectiveTrustPolicyByRepositoryID(context.Context, string) (domain.TrustPolicy, error)
	AssetByRepositoryID(context.Context, string, string) (domain.Asset, error)
	AssetByIDAndRepositoryID(context.Context, string, int64) (domain.Asset, error)
	AssetsByRepositoryID(context.Context, string, string) ([]domain.Asset, error)
	AssetPathsByRepositoryID(context.Context, string, string, string, int) ([]string, error)
	AssetPageByRepositoryID(context.Context, string, AssetPageRequest) (AssetPage, error)
	ComponentVersionPageByRepositoryID(context.Context, string, *ComponentVersion, int) (ComponentVersionPage, error)
	AssetCountByRepositoryID(context.Context, string) (int, error)
	RetentionGroupPageByRepositoryID(context.Context, string, string, int) ([]string, bool, error)
	RetentionGroupAssetsByRepositoryID(context.Context, string, string) ([]domain.Asset, error)
	DirectoryAssetsByRepositoryID(context.Context, string, string) ([]domain.Asset, error)
	SubtreeAssetsByRepositoryID(context.Context, string, string) ([]domain.Asset, error)
	DeleteAssetByRepositoryID(context.Context, string, string, string) (domain.Asset, error)
	DeleteAssetWithCompanionsByRepositoryID(context.Context, string, string, string, []string) (domain.Asset, error)
	DeleteAssetByIDWithCompanionsByRepositoryID(context.Context, string, string, int64, []string) (domain.Asset, error)
	DeleteOCIManifestByRepositoryID(context.Context, string, string, string) ([]domain.Asset, error)
	NegativeCacheHitByRepositoryID(context.Context, string, string, time.Time) (bool, error)
	BeginProxyFetchByRepositoryID(context.Context, string, string, time.Time) (domain.ProxyFetchToken, error)
	PublishProxyNotFoundByRepositoryID(context.Context, string, string, domain.ProxyFetchToken, time.Time) (bool, error)
	PublishProxyNotModifiedByRepositoryID(context.Context, string, string, int64, domain.ProxyFetchToken, time.Time) (bool, error)
	PutNegativeCacheByRepositoryID(context.Context, string, string, time.Time) error
	ClearNegativeCacheByRepositoryID(context.Context, string, string) error
	TouchAssetByRepositoryID(context.Context, string, int64, time.Time) error
	RefreshAssetByRepositoryID(context.Context, string, int64, time.Time) error
	SetAttributesByRepositoryID(context.Context, string, int64, string, map[string]any) error
	DeleteAttributesByRepositoryID(context.Context, string, int64, string) error
}

// ForRepository mints an ID-bound view of one already-resolved repository,
// bound to this store as the backend so the scope travels with the resolved
// entity and cannot be used to mutate a same-name replacement repository.
func (s *SQLStore) ForRepository(repository domain.Repository) RepositoryView {
	return forRepository(s, repository)
}

// forRepository binds metadata operations to the resolved repository's ID over
// the given backend. Consumers reach a view only through Store.ForRepository, so
// a view can never be bound to a raw backend that bypasses the active store.
func forRepository(metadata repositoryBackend, repository domain.Repository) RepositoryView {
	return repositoryScope{metadata: metadata, repository: repository}
}

type repositoryScope struct {
	metadata   repositoryBackend
	repository domain.Repository
}

func (scope repositoryScope) Repository(ctx context.Context) (domain.Repository, error) {
	return scope.metadata.RepositoryByID(ctx, scope.repository.ID)
}

func (scope repositoryScope) DownloadGate(ctx context.Context) (domain.DownloadGate, error) {
	return scope.metadata.DownloadGateByRepositoryID(ctx, scope.repository.ID)
}

func (scope repositoryScope) EffectiveTrustPolicy(ctx context.Context) (domain.TrustPolicy, error) {
	return scope.metadata.EffectiveTrustPolicyByRepositoryID(ctx, scope.repository.ID)
}

func (scope repositoryScope) Asset(ctx context.Context, path string) (domain.Asset, error) {
	return scope.metadata.AssetByRepositoryID(ctx, scope.repository.ID, path)
}

func (scope repositoryScope) AssetByID(ctx context.Context, id int64) (domain.Asset, error) {
	return scope.metadata.AssetByIDAndRepositoryID(ctx, scope.repository.ID, id)
}

func (scope repositoryScope) Assets(ctx context.Context, prefix string) ([]domain.Asset, error) {
	return scope.metadata.AssetsByRepositoryID(ctx, scope.repository.ID, prefix)
}

func (scope repositoryScope) AssetPaths(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	return scope.metadata.AssetPathsByRepositoryID(ctx, scope.repository.ID, prefix, after, limit)
}

func (scope repositoryScope) AssetPage(ctx context.Context, request AssetPageRequest) (AssetPage, error) {
	return scope.metadata.AssetPageByRepositoryID(ctx, scope.repository.ID, request)
}

func (scope repositoryScope) ComponentVersionPage(ctx context.Context, after *ComponentVersion, limit int) (ComponentVersionPage, error) {
	return scope.metadata.ComponentVersionPageByRepositoryID(ctx, scope.repository.ID, after, limit)
}

func (scope repositoryScope) AssetCount(ctx context.Context) (int, error) {
	return scope.metadata.AssetCountByRepositoryID(ctx, scope.repository.ID)
}

func (scope repositoryScope) RetentionGroupPage(ctx context.Context, after string, limit int) ([]string, bool, error) {
	return scope.metadata.RetentionGroupPageByRepositoryID(ctx, scope.repository.ID, after, limit)
}

func (scope repositoryScope) RetentionGroupAssets(ctx context.Context, group string) ([]domain.Asset, error) {
	return scope.metadata.RetentionGroupAssetsByRepositoryID(ctx, scope.repository.ID, group)
}

func (scope repositoryScope) DirectoryAssets(ctx context.Context, directory string) ([]domain.Asset, error) {
	return scope.metadata.DirectoryAssetsByRepositoryID(ctx, scope.repository.ID, directory)
}

func (scope repositoryScope) SubtreeAssets(ctx context.Context, directory string) ([]domain.Asset, error) {
	return scope.metadata.SubtreeAssetsByRepositoryID(ctx, scope.repository.ID, directory)
}

func (scope repositoryScope) DeleteAsset(ctx context.Context, path string) (domain.Asset, error) {
	return scope.metadata.DeleteAssetByRepositoryID(ctx, scope.repository.ID, scope.repository.Name, path)
}

func (scope repositoryScope) DeleteAssetWithCompanions(ctx context.Context, path string, companionPaths []string) (domain.Asset, error) {
	return scope.metadata.DeleteAssetWithCompanionsByRepositoryID(ctx, scope.repository.ID, scope.repository.Name, path, companionPaths)
}

func (scope repositoryScope) DeleteAssetByIDWithCompanions(ctx context.Context, assetID int64, companionPaths []string) (domain.Asset, error) {
	return scope.metadata.DeleteAssetByIDWithCompanionsByRepositoryID(ctx, scope.repository.ID, scope.repository.Name, assetID, companionPaths)
}

func (scope repositoryScope) DeleteOCIManifestByDigest(ctx context.Context, path string) ([]domain.Asset, error) {
	return scope.metadata.DeleteOCIManifestByRepositoryID(ctx, scope.repository.ID, scope.repository.Name, path)
}

func (scope repositoryScope) NegativeCacheHit(ctx context.Context, path string, now time.Time) (bool, error) {
	return scope.metadata.NegativeCacheHitByRepositoryID(ctx, scope.repository.ID, path, now)
}

func (scope repositoryScope) BeginProxyFetch(ctx context.Context, path string, leaseUntil time.Time) (domain.ProxyFetchToken, error) {
	return scope.metadata.BeginProxyFetchByRepositoryID(ctx, scope.repository.ID, path, leaseUntil)
}

func (scope repositoryScope) PublishProxyNotFound(ctx context.Context, path string, token domain.ProxyFetchToken, expiresAt time.Time) (bool, error) {
	return scope.metadata.PublishProxyNotFoundByRepositoryID(ctx, scope.repository.ID, path, token, expiresAt)
}

func (scope repositoryScope) PublishProxyNotModified(ctx context.Context, path string, id int64, token domain.ProxyFetchToken, validatedAt time.Time) (bool, error) {
	return scope.metadata.PublishProxyNotModifiedByRepositoryID(ctx, scope.repository.ID, path, id, token, validatedAt)
}

func (scope repositoryScope) PutNegativeCache(ctx context.Context, path string, expiresAt time.Time) error {
	return scope.metadata.PutNegativeCacheByRepositoryID(ctx, scope.repository.ID, path, expiresAt)
}

func (scope repositoryScope) ClearNegativeCache(ctx context.Context, path string) error {
	return scope.metadata.ClearNegativeCacheByRepositoryID(ctx, scope.repository.ID, path)
}

func (scope repositoryScope) TouchAsset(ctx context.Context, id int64, at time.Time) error {
	return scope.metadata.TouchAssetByRepositoryID(ctx, scope.repository.ID, id, at)
}

func (scope repositoryScope) RefreshAsset(ctx context.Context, id int64, at time.Time) error {
	return scope.metadata.RefreshAssetByRepositoryID(ctx, scope.repository.ID, id, at)
}

func (scope repositoryScope) SetAttributes(ctx context.Context, id int64, namespace string, value map[string]any) error {
	return scope.metadata.SetAttributesByRepositoryID(ctx, scope.repository.ID, id, namespace, value)
}

func (scope repositoryScope) DeleteAttributes(ctx context.Context, id int64, namespace string) error {
	return scope.metadata.DeleteAttributesByRepositoryID(ctx, scope.repository.ID, id, namespace)
}
