// Package format is the public service-provider interface for suxen
// repository formats.
//
// A format teaches the server how a package ecosystem (Maven, npm, ...) maps
// onto suxen's asset model. Formats are compiled into the binary and announce
// themselves with Register, typically from an init function in the format
// package.
//
// A registered format rides the generic asset pipeline: hosted uploads are
// PUT requests carrying the artifact bytes, downloads stream the stored blob,
// proxy repositories fetch the same path from the upstream and cache the
// response, and group repositories serve the first member that resolves the
// path. The optional interfaces below let a format refine that pipeline —
// validate uploads and proxy paths, project coordinate attributes, mark
// mutable proxy paths, merge index content across group members, and
// synthesize hosted index content — without owning the HTTP layer. Formats
// that require their own wire protocol claim request paths through the
// optional WireProtocol hook and serve them over the same asset pipeline via
// WireTools. OCI is still served by the host process directly.
package format

import (
	"context"
	"time"
)

// Repository is the format-relevant view of a repository resource.
type Repository struct {
	Name           string
	Format         string
	Type           string // "hosted", "proxy", or "group"
	Upstream       string // upstream URL for proxy repositories
	AllowOverwrite bool   // effective hosted artifact replacement policy
	// Config carries the resource's formatConfig object. Its schema is owned
	// by the format, which must reject unknown or invalid entries in
	// ValidateRepository.
	Config map[string]any
}

// Asset is the format-relevant view of a stored asset.
type Asset struct {
	Repository  string
	Path        string
	Digest      string
	Size        int64
	ContentType string
	Kind        string
	Reference   string
	// UpdatedAt is when the asset was stored or last revalidated against
	// its upstream. It is zero for projections that do not carry timestamps.
	UpdatedAt time.Time
	// ValidatedAt is the last successful upstream validation. Hosted assets use
	// their content update time.
	ValidatedAt time.Time
}

// Format identifies a repository format. Implementations usually also
// implement one or more of the optional interfaces below.
type Format interface {
	// Name is the format identifier used in repository resources
	// (for example "maven"). It must be a non-empty lowercase identifier,
	// unique per process, and becomes a reserved attribute namespace.
	Name() string
}

// RepositoryValidator lets a format validate repository resources at create,
// update, and provisioning time — including the format's Config schema.
type RepositoryValidator interface {
	ValidateRepository(repository Repository) error
}

// MutableHostedPath identifies hosted metadata files that clients must update
// while publishing new versions. This bypasses the repository overwrite policy
// for those paths only; artifact files remain protected.
type MutableHostedPath interface {
	MutableHostedPath(repository Repository, assetPath string) bool
}

// UploadPolicy lets a format accept or reject hosted uploads by path before
// any content is read. Rejections should be returned as *PolicyViolation so
// clients receive a structured 400 response.
type UploadPolicy interface {
	ValidateUpload(repository Repository, assetPath string) error
}

// AttributeProjector derives the format's coordinate attribute namespace for
// an asset. The projection is computed at read time from authoritative asset
// fields; it is never stored, so it cannot be forged by attribute writers.
// The returned map is published under the format's name (for example
// "maven.groupId") and participates in cleanup predicates, download gates,
// and search.
type AttributeProjector interface {
	ProjectAttributes(asset Asset) map[string]any
}

// ProxyPolicy lets a format mark upstream paths whose content may change over
// time (mutable package indexes, snapshot metadata). Cached assets on mutable
// paths are revalidated against the upstream when older than the configured
// proxy TTL; all other paths are cached immutably.
type ProxyPolicy interface {
	MutableUpstreamPath(repository Repository, assetPath string) bool
}

// ProxyPathPolicy lets a format refuse to serve proxy paths before the cache
// or the upstream is consulted (for example a release-only Maven proxy
// refusing SNAPSHOT paths). Unlike UploadPolicy this should enforce policy,
// not layout: upstreams legitimately serve paths outside the package layout
// (indexes, key files), and those must pass through. Rejections should be
// returned as *PolicyViolation.
type ProxyPathPolicy interface {
	ValidateProxyPath(repository Repository, assetPath string) error
}

// IndexRewriter lets a format rewrite index or metadata content on the way
// out so URLs the upstream embedded point back at this repository (a Cargo
// registry config, an npm packument). repositoryURL is the absolute URL of
// the repository root as the current request reached it, so the same
// instance works under every hostname it is served on. The host calls the
// hook only for proxy paths the format marks mutable through ProxyPolicy and
// for merged group content on such paths, and only for bodies under the
// synthesis size limit; stored bytes stay verbatim and artifact paths are
// never rewritten, which keeps digests equal to the upstream bytes.
type IndexRewriter interface {
	RewriteIndex(
		repository Repository,
		assetPath string,
		body []byte,
		contentType string,
		repositoryURL string,
	) (rewritten []byte, newContentType string, err error)
}

// NegotiatedIndexRewriter extends IndexRewriter to render a mutable proxy
// index or merged group index for the current request's Accept header. The
// host prefers this hook and adds Vary: Accept to
// successful responses. Stored upstream bytes remain unchanged. Implementers
// can parse either cached representation and render the one the client asks
// for, without splitting cache entries by Accept.
type NegotiatedIndexRewriter interface {
	IndexRewriter
	RewriteIndexForAccept(
		repository Repository,
		assetPath string,
		body []byte,
		contentType string,
		repositoryURL string,
		accept string,
	) (rewritten []byte, newContentType string, err error)
}

// ResolvedProxyRequest is the outcome of resolving one proxy read: the opaque
// cache identity to store the response under, and the upstream URL to fetch it
// from. It is produced once and carried through positive-cache lookup,
// negative-cache lookup, and fetch. The original asset path remains the format
// path for policy, index rewriting, and coordinates.
type ResolvedProxyRequest struct {
	// CachePath is the repository-scoped opaque identity the response is
	// stored and looked up under. It must be non-empty and must not contain
	// credentials or other secret query bytes. Distinct upstream targets must
	// map to distinct cache paths. When ExpectedDigests is set, a change to
	// that digest set must also change CachePath: cache hits do not rehash bytes.
	CachePath string
	// UpstreamURL is the absolute URL to fetch the asset from. "" means the
	// host's default construction (the repository upstream joined with the
	// asset path). The host does not append rawQuery to that default; a format
	// with query-selected content must return an explicit URL and cache key.
	UpstreamURL string
	// ExpectedDigests optionally verifies a downloaded representation before
	// it enters the cache. Each entry is a lowercase algorithm:hex digest;
	// supported algorithms are sha1, sha256, sha384, and sha512. Entries must
	// use the same algorithm and are alternatives: any match accepts the body.
	// A format should select its strongest advertised algorithm before filling
	// this list. The host rejects unsupported or malformed entries.
	ExpectedDigests []string
	// CacheOnly permits a valid cached response but forbids an upstream fetch.
	// A cache miss or a cached asset requiring revalidation resolves as absent.
	// This is useful when a format can identify immutable cached bytes but no
	// longer has the metadata needed to construct a safe upstream request.
	CacheOnly bool
}

// ProxyRequestResolver resolves one proxy read before positive or negative
// cache lookup. The host passes the original asset path and exact raw query;
// formats can validate signed URLs, choose an opaque cache identity, and
// select another upstream host in one invocation. Return a *PolicyViolation
// to reject the read before either cache or upstream is consulted. The host
// uses the original path for policy, rewriting, and coordinates, and uses
// CachePath only for storage. An empty UpstreamURL selects the repository
// upstream joined with the original path, without appending rawQuery. Formats
// without this capability use the original path as the cache key and the
// default upstream URL.
// stored exposes only this proxy repository's cached assets. Independent path
// policy, authorization, download gates, outbound validation, and upstream
// credential origin protection still apply.
type ProxyRequestResolver interface {
	ResolveProxyRequest(
		ctx context.Context,
		repository Repository,
		assetPath string,
		rawQuery string,
		stored StoredAssets,
	) (resolved ResolvedProxyRequest, err error)
}

// GroupMerger lets a format merge index content across group members instead
// of serving the first member that resolves (Maven merges
// maven-metadata.xml). Merged responses are synthesized per request and never
// stored.
type GroupMerger interface {
	// GroupMergeSource maps a requested path to the member path whose
	// contents must be collected for merging. It returns ok=false for paths
	// that resolve first-match. The source usually equals the requested path;
	// derived paths differ (a metadata checksum is computed from the merged
	// metadata, so its source is the metadata path itself).
	GroupMergeSource(repository Repository, assetPath string) (sourcePath string, ok bool)
	// MergeGroupContent merges the member contents of the source path — in
	// member order, malformed entries included — and renders the response for
	// the requested path. It is called with at least one source and runs for
	// single sources too, so that derived paths (checksums) stay consistent
	// with the rendered merge output.
	MergeGroupContent(
		repository Repository,
		assetPath string,
		sources [][]byte,
	) (content []byte, contentType string, err error)
}

// GroupSourceNormalizer resolves member-specific context before independent
// index documents are merged. It is useful for relative links whose base URL
// belongs to the source repository rather than the group.
type GroupSourceNormalizer interface {
	NormalizeGroupSource(repository Repository, assetPath string, body []byte) ([]byte, error)
}

// GroupArtifactSelector keeps an artifact's supplying member aligned with a
// merged index when the format gives multiple members the same artifact path.
// The host reads each member's current source in group order before resolving
// the artifact. A member whose source does not advertise it is skipped; the
// first advertising member owns the path even if its artifact fetch fails.
// If no member serves the source, ordinary first-match resolution applies so
// direct artifact requests still work without a published index.
type GroupArtifactSelector interface {
	// GroupArtifactSource maps an artifact request to the index path used to
	// decide member ownership. Return ok=false for ordinary first-match paths.
	GroupArtifactSource(repository Repository, assetPath string) (sourcePath string, ok bool)
	// GroupSourceContainsArtifact reports whether this member's source
	// advertises the artifact. A malformed source should return an error.
	GroupSourceContainsArtifact(repository Repository, sourcePath, assetPath string, body []byte) (bool, error)
}

// GroupArtifactLocator selects artifacts whose public URL does not identify
// their index path or filename. Candidates are discovered from stored member
// indexes, then each candidate source is refreshed before an HTTP download.
// An artifact key's first advertising member owns it even if the requested
// URL belongs to a later member. Inventory uses stored sources only.
type GroupArtifactLocator interface {
	// handled identifies artifact paths governed by this locator. A handled
	// path with no candidate has no visible owner and must not fall through.
	GroupArtifactCandidates(ctx context.Context, repository Repository, request GroupArtifactRequest, stored StoredAssets) (candidates []GroupArtifactCandidate, handled bool, err error)
	// GroupSourceArtifact checks artifact-key ownership in a current source.
	// matches additionally requires the requested URL or stored generation to
	// be the one this source advertises.
	GroupSourceArtifact(repository Repository, sourcePath, artifactKey string, request GroupArtifactRequest, body []byte) (owns, matches bool, err error)
}

// GroupArtifactRequest carries the public artifact path and exact HTTP query.
// StoredPath is set only for inventory and ID visibility checks, where the
// stored generation must match the current index entry.
type GroupArtifactRequest struct {
	Path       string
	RawQuery   string
	StoredPath string
}

// GroupArtifactCandidate identifies an index entry by source and logical key;
// the URL associated with that key may differ between members.
type GroupArtifactCandidate struct {
	SourcePath  string
	ArtifactKey string
}

// StoredAssets is the capability handle a HostedSynthesizer uses to inspect
// hosted assets and a ProxyRequestResolver uses to inspect proxy cache entries.
// It is always scoped to the repository being served.
type StoredAssets interface {
	// VisitAssetPaths visits stored paths beginning with prefix in ascending
	// lexical order. Returning false stops successfully; a callback error stops
	// and is returned. Visits are bounded in host memory, but a concurrent
	// mutation can change which paths remain in a later batch. On any error,
	// including cancellation, earlier visits remain observable.
	VisitAssetPaths(ctx context.Context, prefix string, visit func(path string) (continueVisit bool, err error)) error
	// ReadAsset returns the content of one stored asset, bounded by the
	// server's synthesis size limit. found is false when no asset exists at
	// the path.
	ReadAsset(ctx context.Context, assetPath string) (content []byte, found bool, err error)
}

// HostedSynthesizer lets a format answer hosted reads for paths that have no
// stored asset by deriving content from the assets that do exist (Maven
// synthesizes maven-metadata.xml and its checksums). Stored assets always
// win: the hook only runs on misses, and its output is served per request,
// never stored.
type HostedSynthesizer interface {
	SynthesizeHosted(
		ctx context.Context,
		repository Repository,
		assetPath string,
		assets StoredAssets,
	) (content []byte, contentType string, ok bool, err error)
}

// RetentionGrouping lets a format own how its assets group for keepLast
// retention and which companion (non-artifact) records must be deleted with an
// artifact, instead of the host hard-coding that per-format coordinate
// knowledge. Optional; a format that does not implement it keeps the host's
// default behavior (directory grouping, plus the host-owned OCI manifest
// grouping).
type RetentionGrouping interface {
	// RetentionGroupKey returns the identity whose versions compete for
	// keepLast — every release of one package shares a key. ok=false falls
	// back to the host's default grouping for that asset; it does not force
	// directory grouping onto assets the host groups specially. The host
	// stores the key when the asset is written and recomputes it when the
	// repository configuration changes, so it must depend only on the
	// repository configuration and the asset's stored fields.
	RetentionGroupKey(repository Repository, asset Asset) (key string, ok bool)

	// CompanionPaths returns the repository-relative paths of the
	// exclusively-owned per-artifact metadata records that must be deleted
	// atomically with the artifact at assetPath (a Cargo per-version index
	// entry, an npm per-version metadata document) — never a shared index.
	// The host validates each path (repository-relative, no traversal),
	// deletes only records it confirms are metadata, and owns the deletion,
	// event, and accounting; a declaration cannot make another artifact
	// disappear. An empty result means no companions.
	CompanionPaths(repository Repository, assetPath string) []string
}

// RetentionGroupingRevision optionally versions a format's RetentionGroupKey.
// The host stores every asset's group key when the asset is written; when the
// revision a format reports differs from the one the stored keys were
// computed with, the host recomputes them at startup. A format must change
// the revision whenever RetentionGroupKey would return a different key for an
// asset that is already stored; otherwise those assets keep their old group.
type RetentionGroupingRevision interface {
	RetentionGroupingRevision() string
}

// RetentionUnitPaths optionally identifies the complete set of stored artifact
// paths that make one independently usable version. For a repository,
// cleanup considers the unit only when every declared path exists and matches
// the policy predicates. It counts complete units for keepLast and deletes all
// their unchanged rows atomically. An empty result uses ordinary asset cleanup.
// Paths must be repository-relative, distinct, and include assetPath.
type RetentionUnitPaths interface {
	RetentionUnitPaths(repository Repository, assetPath string) []string
}

// RetentionUnitDirectory identifies a version whose set of files is variable.
// Every direct child of the returned repository-relative directory belongs to
// one unit; cleanup requires every child to match the policy and deletes the
// entire observed directory atomically only if no child changed or appeared.
// The directory must be the direct parent of assetPath. An empty result uses
// ordinary asset cleanup. Formats should return a directory for every member,
// including checksums and metadata stored inside the version directory.
type RetentionUnitDirectory interface {
	RetentionUnitDirectory(repository Repository, assetPath string) string
}

// RetentionUnitAnchor optionally requires a concrete artifact to seed a
// directory unit. Other declared direct children remain part of that unit,
// but cannot form one on their own. Formats without this interface retain the
// RetentionUnitDirectory behavior above.
type RetentionUnitAnchor interface {
	IsRetentionUnitAnchor(repository Repository, assetPath string) bool
}

// PolicyViolation is a structured rejection raised by a format hook. The
// server maps it to an HTTP 400 problem response carrying Code and Message.
type PolicyViolation struct {
	// Code is a stable, snake_case machine-readable identifier
	// (for example "maven_version_policy").
	Code string
	// Message is a human-readable explanation of the rejection.
	Message string
}

func (violation *PolicyViolation) Error() string {
	return violation.Message
}
