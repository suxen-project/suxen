package format

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WireTools publication errors can be inspected with errors.Is, including
// when the host wraps an underlying failure. Formats should map these
// categories to their native client responses and keep unknown errors private.
var (
	ErrConflict       = errors.New("immutable publication conflict")
	ErrPolicyRejected = errors.New("publication rejected by policy")
	ErrUploadLimit    = errors.New("publication exceeds upload limit")
)

// WireProtocol lets a format serve its own HTTP protocol for the request
// paths it claims, instead of the generic path-addressed asset pipeline. The
// host keeps everything around the protocol: repository lookup, the
// /repository/{name}/ prefix, principal resolution and the privilege check,
// storage, hashing, and the hardened outbound client. The format sees the
// original request for its claimed paths and reaches the asset pipeline through
// WireTools. A compiled-in format is trusted application code, not an isolation
// boundary: it can inspect request headers and the repository's upstream URL.
// Everything a wire format stores is still an ordinary asset with a path, so
// the other hooks (AttributeProjector, ProxyPolicy, ...) keep applying.
type WireProtocol interface {
	// WireAction claims a repository-relative request path for the format's
	// protocol and names the repository privilege the host checks before
	// dispatch: "read", "write", or "delete". ok=false hands the request to
	// the generic asset pipeline. An empty action dispatches without a
	// privilege check, for protocol endpoints that must answer anonymous
	// clients (a capability advertisement); the format must not serve
	// repository content from such a path.
	WireAction(
		repository Repository,
		method string,
		requestPath string,
		query url.Values,
	) (action string, ok bool)
	// ServeWire serves one claimed request after the host has checked the
	// declared privilege. r is the original request and may contain credentials;
	// repository includes the configured upstream URL. The format must treat both
	// as sensitive. WireTools is the supported path to host-managed storage,
	// policy enforcement, and hardened upstream access.
	ServeWire(
		w http.ResponseWriter,
		r *http.Request,
		repository Repository,
		requestPath string,
		tools WireTools,
	)
}

// WireTools is the capability handle a WireProtocol uses to reach the asset
// pipeline of the repository it serves. Paths are repository-relative asset
// paths, exactly like the ones the generic pipeline uses.
type WireTools interface {
	// MaxUploadBytes is the host's per-request upload budget. Wire protocols
	// must apply it before buffering, allocating, or parsing request content.
	MaxUploadBytes() int64
	// VisitAssetPaths visits stored paths beginning with prefix in ascending
	// lexical order. Returning false stops successfully; a callback error stops
	// and is returned. Visits are bounded in host memory, but a concurrent
	// mutation can change which paths remain in a later batch. On any error,
	// including cancellation, earlier visits remain observable.
	VisitAssetPaths(ctx context.Context, prefix string, visit func(path string) (continueVisit bool, err error)) error
	// StatAsset returns the stored asset at the path without its content.
	// found is false when nothing is stored there.
	StatAsset(ctx context.Context, assetPath string) (asset Asset, found bool, err error)
	// OpenAsset opens the stored asset's content for streaming. The caller
	// closes the reader. found is false when nothing is stored there.
	OpenAsset(ctx context.Context, assetPath string) (content io.ReadCloser, asset Asset, found bool, err error)
	// OpenMetadataAsset opens format-owned index metadata. This explicit
	// exemption is only for content used to synthesize an index; artifact bytes
	// must always use OpenAsset and pass repository download policy.
	OpenMetadataAsset(ctx context.Context, assetPath string) (content io.ReadCloser, asset Asset, found bool, err error)
	// StoreAsset hashes and stores content at a path and returns the stored asset.
	// Repeating the same digest is idempotent. A hosted repository permits a
	// different digest only when allowOverwrite is enabled; a proxy still uses
	// immutable cache semantics. The host
	// enforces its upload size limit, dedups by digest, and emits the usual asset
	// events. Expected failures wrap ErrConflict, ErrPolicyRejected, or
	// ErrUploadLimit. For a proxy repository this fills the format's cache.
	StoreAsset(ctx context.Context, assetPath string, contentType string, content io.Reader) (Asset, error)
	// StoreMetadataAsset stores format-owned index metadata. Metadata is not a
	// downloadable artifact and is exempt from artifact signature policy.
	StoreMetadataAsset(ctx context.Context, assetPath string, contentType string, content io.Reader) (Asset, error)
	// StoreAssets validates and stages all inputs, then exposes every path in one
	// metadata transaction. Metadata marks an explicit format-index exemption.
	StoreAssets(ctx context.Context, assets []AssetInput) ([]Asset, error)
	// ProxyTTL is the configured revalidation interval for mutable proxy
	// content. Zero means revalidate on every request.
	ProxyTTL() time.Duration
	// Upstream sends one request to the proxy upstream through the host's
	// hardened outbound client, carrying the upstream credentials. The path
	// (which may include a query string) is relative to the repository's
	// upstream URL. It fails for repositories that are not proxies. The
	// caller closes the response body.
	Upstream(
		ctx context.Context,
		method string,
		upstreamPath string,
		header http.Header,
		body io.Reader,
	) (*http.Response, error)
}

// ReportWireError records an unexpected wire-protocol failure with the host's
// request context when the host supports reporting. Formats should call this
// once at the response boundary, then send a stable, generic native-protocol
// message. The optional capability keeps existing WireTools implementations
// source-compatible.
func ReportWireError(tools WireTools, ctx context.Context, operation string, err error) {
	if reporter, ok := tools.(interface {
		ReportWireError(context.Context, string, error)
	}); ok && err != nil {
		reporter.ReportWireError(ctx, operation, err)
	}
}

// AssetInput is one member of an atomic wire-protocol publication.
type AssetInput struct {
	Path        string
	ContentType string
	Content     io.Reader
	Metadata    bool
	// Replace permits a different digest to replace a proxy cache path or
	// hosted format metadata. Hosted artifact replacement follows the
	// repository allowOverwrite policy; metadata must set Replace explicitly
	// when it is a mutable index rather than a per-version identity.
	Replace bool
	// ImmutableIdentity marks a format-owned stable identity claim that must
	// never change digest, even when hosted allowOverwrite is enabled.
	ImmutableIdentity bool
}
