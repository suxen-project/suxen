# Extending suxen: plugins and the SPI

suxen is extended at compile time. Plugins are ordinary Go packages that
register a driver or a repository format from an `init` function; a binary
supports exactly the plugins linked into it. There is no runtime plugin
loading — a compiled-in registry keeps the binary static, portable, and free
of dynamic-linking constraints. Plugins execute in the server process as
trusted application code. They are not sandboxed and must receive the same
review and operational trust as the server itself.

Public service-provider interfaces (SPIs) are importable by out-of-tree
code:

| Package | Extends | Reference implementation |
|---|---|---|
| `github.com/suxen-project/suxen/spi/blob` | Blob storage backends | `plugins/blobstore/gcs` |
| `github.com/suxen-project/suxen/spi/format` | Repository formats | `plugins/format/maven` |
| `github.com/suxen-project/suxen/spi/api` | Control-plane routes under `/api/v1/plugins/{pluginID}/` | — |

Each SPI is reported through
[`internal/contract/versions.yaml`](../../internal/contract/versions.yaml)
(`blobstore-spi`, `format-spi`, `plugin-api-spi`). During prerelease, these
surfaces share `1.0.0-rc.2` and may still change. Stable SPI versioning begins
with `v1.0.0`. See the
[compatibility policy](../reference/compatibility.md#contract-surface-versions).

Everything under `internal/` remains private and carries no compatibility
promise. The `raw` and `oci` formats and the `fs` and `s3` blob drivers are
built into the server core. `oci` is dispatched through the same
`WireProtocol` hook described below, but as a core format compiled into the
server: its token endpoint, upload sessions, and bound-port listeners stay
host-owned, and the name stays reserved so a plugin cannot shadow it.

## The default plugin set

The default build — `make build`, the published container image — includes
every plugin under `plugins/`:

| Plugin | Registers | Exclude with build tag |
|---|---|---|
| `plugins/blobstore/gcs` | `gcs` blob driver (`gcs://bucket/prefix`) | `suxen_no_gcs` |
| `plugins/format/maven` | `maven` repository format | `suxen_no_maven` |
| `plugins/format/git` | `git` repository format (snapshot proxy) | `suxen_no_git` |
| `plugins/format/gomod` | `go` repository format (module proxy) | `suxen_no_go` |
| `plugins/format/cargo` | `cargo` repository format (sparse registry proxy and hosted publish) | `suxen_no_cargo` |
| `plugins/format/npm` | `npm` repository format (registry proxy and hosted publish) | `suxen_no_npm` |
| `plugins/format/pypi` | `pypi` repository format (simple index proxy and hosted publish) | `suxen_no_pypi` |

Build tags subtract plugins from a build:

```sh
SUXEN_BUILD_TAGS=suxen_no_gcs,suxen_no_maven,suxen_no_git,suxen_no_go,suxen_no_cargo,suxen_no_npm,suxen_no_pypi make build-server   # core only
docker build --build-arg SUXEN_BUILD_TAGS=suxen_no_maven .       # image without maven
```

A binary without a plugin rejects its configuration up front: repository
creation fails with `invalid_format` for an absent format, and
`SUXEN_BLOBSTORE` rejects URL schemes no compiled-in driver claims.

## Writing a blob storage driver

A driver implements `blob.Store` (six methods: `Put`, `Get`, `Head`, `Walk`,
`Delete`, `Ready`) and optionally `blob.UploadStore` for replica-safe chunked
upload sessions. Register it from `init`:

```go
package mydriver

import "github.com/suxen-project/suxen/spi/blob"

func init() {
	blob.Register(blob.Driver{
		Name:      "mydriver",
		URLScheme: "mydriver", // enables SUXEN_BLOBSTORE=mydriver://...
		Open: func(configuration string) (blob.Store, error) {
			return newStore(configuration)
		},
		// Normalizes configuration aliases so the server can detect two
		// resources addressing the same physical storage. Exclude
		// credentials from the canonical form.
		CanonicalConfiguration: canonicalize,
		// True only when every replica sees the same storage; gates
		// multi-replica deployments.
		SharedStorage: true,
	})
}
```

The contract is documented on the `spi/blob` types. Validate the
implementation with the exported conformance suite — the same suite the
built-in drivers run:

```go
func TestConformance(t *testing.T) {
	blobtest.RunStoreSuite(t, func(t *testing.T) blob.Store {
		return newTestStore(t)
	})
}
```

`examples/plugin-memblob` is a complete, buildable out-of-tree module. Its
main package imports `suxencmd.Main` and the default plugin set, its driver runs
the blob conformance suite, and its `exampleplugin` package compiles small
format and control-plane route consumers. `make check-example` (part of
`make check` and CI) verifies its manifest is tidy, then builds, vets, and tests
the separate module. An SPI change that breaks any of those external import or
implementation surfaces therefore fails the build.

## Writing a repository format

A format implements `format.Format` (just `Name() string`) plus any of the
optional hooks:

- `RepositoryValidator` — validate repository resources, including the
  format-owned `formatConfig` object (its schema belongs to the format).
- `UploadPolicy` — accept or reject hosted upload paths before content is
  read (layout enforcement, version policies).
- `AttributeProjector` — derive the format's coordinate attributes
  (`maven.groupId`, ...) from authoritative asset fields at read time. The
  format name is a reserved attribute namespace; projected coordinates feed
  cleanup predicates, download gates, search, and webhooks.
- `RetentionGrouping` — own how the format's assets group for `keepLast`
  retention and which companion metadata records must be deleted with an
  artifact, instead of the host hard-coding that coordinate knowledge. Cargo
  groups every version of a crate under its download prefix and ties each crate
  file to its per-version index entry; npm groups by package and ties each
  tarball to its per-version metadata document. A format that does not
  implement it keeps the host default (directory grouping, plus the host-owned
  OCI manifest grouping). The host validates the declared companion paths,
  deletes the unchanged artifact and records currently classified as metadata
  at those paths in one transaction, and owns the deletion event and accounting.
  Tests cover a companion committed after selection but before deletion begins.
  The declaration does not coordinate independent metadata writes: publish the
  artifact and its exclusively owned companions together using `StoreAssets`.
  A format must not declare shared indexes as companions. The host accepts at
  most 16 declared paths, each at most 512 bytes with no absolute prefix, empty
  segments, or dot segments; invalid declarations preserve the artifact.
  The host stores each asset's group key when the asset is written and
  recomputes it when the repository configuration changes; cleanup then reads
  one group at a time, so the key must depend only on the repository
  configuration and the asset's stored fields. Unit declarations must stay
  reciprocal, because cleanup evaluates a group with the directories of its
  assets and of their declared unit paths, not the whole repository.
  By default, `keepLast` counts matching asset rows by update time within each
  group. An empty companion list declares no companions; returning `ok=false`
  for a group key uses the host fallback.
- `RetentionGroupingRevision` — optionally version `RetentionGroupKey`. The
  host records the revision each repository's stored group keys were computed
  with and recomputes them at startup when the running format reports another.
  Change the revision in any release whose `RetentionGroupKey` returns a
  different key for an asset that is already stored; otherwise those assets
  keep their old group and a policy no longer reaches them.
- `RetentionUnitPaths` — optionally declare every artifact path required for
  one usable version. Cleanup only considers a complete unit when all of its
  existing rows match the predicates; `keepLast` then counts whole units within
  each retention group. The host compares and deletes every row in one
  transaction, so a concurrent change to any member preserves the entire
  unit. Incomplete units are left for explicit deletion. The declaration must
  be stable and reciprocal from every member, contain its own path, and use
  distinct repository-relative paths (at most 16). Go modules use this for
  their `.info`, `.mod`, and `.zip` files.
- `RetentionUnitDirectory` — declare a version directory whose direct child
  files vary, such as a Maven release with optional classifiers, checksums,
  and signatures. Return the same repository-relative parent directory from
  every child. The host validates the declaration, requires every stored
  direct child to match all policy predicates, and counts the directory as
  one `keepLast` unit. It checks the complete path set and every row again in
  one deletion transaction under the publication lock, so a newly published
  child or changed row preserves the whole directory. A declaration for a
  different parent or an unsafe path preserves the calling asset. Directory
  units take precedence over `RetentionUnitPaths` for assets they claim if a
  format implements both. Maven treats one base-version `-SNAPSHOT` directory
  as one unit, including any timestamped builds and stored version metadata.
- `RetentionUnitAnchor` — optionally require a concrete artifact before a
  directory can become a retention unit. Only anchor files establish the unit
  and its retention group; declared companion files still participate in every
  predicate and atomic deletion check. Without an anchor, files follow ordinary
  retention. Formats without this capability retain the default directory-unit
  behavior. Maven uses this to prevent standalone metadata from being mistaken
  for a version solely because its parent name ends in `-SNAPSHOT`.
- `ProxyPolicy` — mark upstream paths as mutable (package indexes, snapshot
  metadata) so proxy repositories revalidate them on the proxy TTL instead of
  caching them forever.
- `ProxyPathPolicy` — refuse proxy paths before the cache or upstream is
  consulted (a release-only Maven proxy rejecting SNAPSHOT paths). Enforce
  policy, not layout: upstreams legitimately serve paths outside the package
  layout.
- `IndexRewriter` — rewrite a proxy index on the way out so URLs the upstream
  embedded point back at this repository (Cargo rewrites the registry
  `config.json`). The host passes the repository URL as the current request
  reached it, calls the hook only on paths the format marks mutable (and on
  merged group content of such paths), and keeps the stored bytes verbatim,
  so digests still equal the upstream's.
- `NegotiatedIndexRewriter` — render a mutable proxy or merged
  group index for the current request's `Accept` header. `RewriteIndexForAccept`
  receives the same inputs as `RewriteIndex`, plus the header value. The host
  prefers this optional capability over `IndexRewriter` and adds `Vary: Accept`.
  Stored bytes remain unchanged; the format must be able to render the requested
  representation from whichever index representation is cached. Existing format
  implementations need no changes.
- `ProxyRequestResolver` — resolve one proxy read in a single call, returning
  an opaque cache identity and optional absolute upstream URL. The host calls
  it once before positive and negative cache lookup, with the original path
  and exact raw query. Return a `*format.PolicyViolation` to reject the read
  before a cache hit or upstream request. `CachePath` must be nonempty and
  contain no credentials or raw signed query bytes; distinct upstream targets
  need distinct keys. The original path still governs format policy, index
  rewriting, and coordinates. An empty upstream URL means the configured
  repository upstream joined with that original path, without forwarding the
  request's raw query. Formats with query-selected content must return an
  explicit target and distinct cache key. Formats without this
  hook use the original path as the cache key and the default upstream URL.
  The host validates resolved targets and sends upstream credentials only to
  the configured upstream origin. Path policy, authorization, and download
  gates apply independently.
  `ExpectedDigests` can require an integrity check before
  a cache fill is published. Entries are lowercase `algorithm:hex` digests
  using one algorithm (`sha1`, `sha256`, `sha384`, or `sha512`); any matching
  entry accepts the body. Select the strongest advertised algorithm and use
  an opaque cache key that changes when the expected digest set changes.
  `CacheOnly` serves a valid cached entry but returns a miss
  if it is absent or needs revalidation, without contacting the upstream. Use
  it when a format can identify retained immutable bytes but can no longer
  resolve a safe upstream URL from deleted registry metadata.
- `GroupMerger` — merge index content across group members instead of serving
  the first match (Maven merges `maven-metadata.xml` and derives its
  checksums from the merged document). Merged responses are synthesized per
  request and never stored.
- `GroupSourceNormalizer` — resolve member-specific context before merging
  independent group source documents, such as relative links in a PyPI index
  whose base URL belongs to the source repository.
- `GroupArtifactSelector` — choose the member supplying an artifact from the
  same current index documents used for group merging. The host checks members
  in order and does not fall through after the first source that advertises
  the artifact. Formats using this hook fail a merged index if any member's
  source could not be read, since a partial index can disagree with cached
  artifact bytes. If no member serves the index path, artifact resolution
  retains first-match behavior for direct requests.
- `GroupArtifactLocator` — discover an artifact's index path
  and filename from stored member indexes when the download URL cannot identify
  them. The host reads the candidate index across members and gives the first
  member advertising that filename ownership, then checks the requested URL
  and query against that member. Recognized artifact paths without a candidate
  do not fall through to another cached file. Inventory checks use stored
  indexes and the opaque cache identity without fetching upstream content.
  This keeps PyPI signed URLs, metadata companions, and cached generations
  aligned with the merged index.

- `MutableHostedPath` — identify mutable hosted indexes and
  their companions that must remain updateable when the repository disallows
  artifact replacement. This exemption is for index maintenance, not package
  payloads or per-version metadata. `Repository.AllowOverwrite` exposes the
  effective hosted policy; the host checks the current value transactionally
  when publishing assets.
- `HostedSynthesizer` — answer hosted reads that have no stored asset by
  deriving content from the assets that exist, through a capability handle
  that lists and reads the repository's assets (Maven synthesizes missing
  `maven-metadata.xml`). Stored assets always win; the hook only runs on
  misses.
- `WireProtocol` — serve the format's own HTTP protocol for the request paths
  it claims (the git plugin serves smart HTTP v2 this way). `WireAction`
  claims a repository-relative path and names the privilege (`read`, `write`,
  `delete`) the host checks before dispatch; `ServeWire` then gets the raw
  request plus a `WireTools` handle that lists, reads, and stores assets,
  exposes the proxy TTL, and sends requests to the proxy upstream through the
  hardened outbound client (the npm plugin serves `npm publish` and its hosted
  packument this way). Everything a wire format stores is still an asset with a
  path, so the other hooks keep applying, and the host keeps ownership of the
  `/repository/{name}/` prefix, authentication, storage, and egress policy.

For a proxy read, `ProxyPathPolicy` runs first, then `ProxyRequestResolver`.
PyPI uses a hash of the exact query for cache separation and preserves the
original query when resolving signed artifact URLs. Cargo and npm use the
same hook to select download hosts advertised in cached registry metadata.

`ServeWire` receives the original `*http.Request`, including its headers, and
the in-process repository view includes the configured upstream URL. Treat credentials in
both as sensitive; public repository API responses remove URL user information, queries,
and fragments. The privilege check and `WireTools` capabilities give a
plugin the supported host services; they do not sandbox compiled Go code or
prevent it from using process memory, the filesystem, or the network directly.

Registered formats ride the generic asset pipeline (path-addressed GET, PUT,
DELETE; proxy fetch-through; group first-match unless a `GroupMerger` claims
the path) unless a `WireProtocol` claims the path.

Rejections should be returned as `*format.PolicyViolation`, which the server
maps to a structured HTTP 400 problem response with the violation's code.

`WireTools` returns public error categories for immutable-content conflicts
(`format.ErrConflict`), repository policy rejection (`format.ErrPolicyRejected`),
and upload limits (`format.ErrUploadLimit`). A native wire protocol can inspect
them with `errors.Is` and use its own response envelope, typically HTTP 409,
403, or 413. Unknown failures should use a generic server error response;
underlying storage or infrastructure error text must not be sent to clients.
A byte-identical publish at an immutable path remains idempotent. Hosted publication
follows the repository's `allowOverwrite` policy; proxy cache inputs retain their
explicit `AssetInput.Replace` behavior. For hosted mutable indexes, use both
`Metadata: true` and `Replace: true`; per-version companion metadata must not use
that exemption. `AssetInput.ImmutableIdentity` protects stable
format identity claims even when artifact replacement is enabled. Store such
claims with the artifact in the same `StoreAssets` call, and declare them as
companions for deletion and retention.
When a wire handler encounters an unexpected failure, call
`format.ReportWireError(tools, r.Context(), operation, err)` once at the
response boundary, then return a stable message in the native protocol.
The host records the underlying cause with request, repository, and format
context. Reporting is an optional capability of `WireTools`; external
implementations do not need to add a method.

`StoredAssets.VisitAssetPaths` and `WireTools.VisitAssetPaths` enumerate
repository-relative paths with a literal prefix in ascending lexical order.
The host reads at most 128 paths per storage query. The callback returns
`(true, nil)` to continue, `(false, nil)` to stop successfully, or an error to
stop and return that error. Cancellation is checked between paths and queries.
Earlier callback effects remain when iteration stops or fails. Concurrent
changes are not snapshotted: a new path after the current cursor may appear,
while a removed path may disappear. A removed cursor path does not break the
next batch. Format code should retain only the fields needed for its response;
generated indexes such as npm packuments and Maven metadata still grow with
the number of versions they must represent.

## Plugin control-plane routes

Plugin HTTP is mounted at `/api/v1/plugins/{pluginID}/{endpoint}`. `pluginID`
must be a lowercase identifier. The dedicated namespace keeps plugin routes
independent of core resource paths. Register panics on duplicate plugin IDs
or conflicting route patterns. `Route.Path` is relative to the plugin ID: use
`""` for the plugin root, and do not use `"/"` as an alias. Nonempty paths
have no leading or trailing slash, empty or `.`/`..` segments, or percent
escapes. Static segments use URL unreserved characters (letters, digits,
`-`, `_`, `.`, `~`); whole `{name}` segments capture parameters with unique
lowercase identifier names. `Route.Methods` contains distinct uppercase
`GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, or `TRACE` values.
Extension methods and `CONNECT` are not supported by this OpenAPI route API.

```go
func init() {
	api.Register("myplugin", api.Route{
		Path:    "rebuild",
		Methods: []string{http.MethodPost},
		Handle: func(ctx api.Context) {
			ctx.WriteJSON(http.StatusOK, map[string]string{"ok": "true"})
		},
		Operations: map[string]api.Operation{
			http.MethodPost: {
				"responses": map[string]any{
					"200": map[string]any{"description": "Rebuild started", "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{
							"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "string"}},
						}},
					}},
				},
			},
	})
}
```

That serves `POST /api/v1/plugins/myplugin/rebuild`. Privilege checks use
`admin:plugin-{pluginID}:read` or `:write` from the method before the route table,
so unknown plugin paths still return 401/403 rather than disclosing whether
the route exists. Cookie-authenticated mutations get the same CSRF
protection as core routes. `Context.DecodeJSON` reads a JSON body under the
same size limit core routes apply; prefer it over reading `Request().Body`
directly. When several routes match, static segments win over `{param}`
segments regardless of registration order, and two paths that differ only in
parameter names are rejected as duplicates.

The registry copies methods and nested OpenAPI operations at registration.
`api.Routes` and `api.Match` also return independent copies, so changing a
returned route does not change later matches or the served document.

Plugin routes appear in the served `/api/openapi.json`. Set
`Route.Operations` (keyed by HTTP method) to an OpenAPI operation object for
every served method, with at least one explicit 2xx response. The host adds
`x-suxen-required-privilege`, the path
parameters it derives from `{name}` segments, and the shared 401/403/default
responses. Describe request and response body schemas inline — plugins cannot add to
the document's components.

Plugin routes have no access to the calling principal or to host state
beyond what the plugin package holds itself.

## Scheduled jobs

Garbage collection (`gc`) and cleanup are host-owned scheduled jobs, not a
plugin seam. Their core paths are separate from plugin paths: a plugin named
`gc` mounts under `/api/v1/plugins/gc/`, while `POST /api/v1/gc` invokes
the gc job (`admin:gc:run`); cleanup policy CRUD and per-repository
`POST /api/v1/repositories/{name}/cleanup` are core routes.

## Building a custom distribution

A custom binary is a small main package that blank-imports the desired
plugins and calls the importable server entrypoint:

```go
package main

import (
	_ "example.com/my/plugin"                          // your plugin
	_ "github.com/suxen-project/suxen/plugins/builtin" // default plugin set

	"github.com/suxen-project/suxen/suxencmd"
)

func main() { suxencmd.Main() }
```

Compile it like any Go module. The whole server is recompiled with your
plugin — that is the model, and there is no ABI to match. Compilation checks
that the selected plugin and server source versions agree on the SPI. Within a
stable major release, adding a required method to an interface implemented by
plugins is a breaking change, just like removing or changing an existing
method. New behavior should use a new optional interface so existing plugin
implementations continue to compile.

### The suxen-sdk image

The version-matched `ghcr.io/suxen-project/suxen-sdk` builder image packages the
Suxen sources with a `suxen-build` helper that generates the main package above and
compiles it. Match its tag to the server version whose SPI contract you target:

```sh
docker run --rm \
    -v "$PWD/my-plugin:/work" \
    -v "$PWD/dist:/out" \
    ghcr.io/suxen-project/suxen-sdk:1.0.0-rc.2 \
    --plugin example.com/my/plugin=/work --version 1.0.0-rc.2-custom
```

Release CI builds the image for amd64 and arm64, scans it for high and critical
vulnerabilities, signs its immutable digest, and attaches provenance. The same CI gate
uses it to compile `examples/plugin-memblob`, then boots that custom binary with the
plugin's `mem://` store and checks readiness and its embedded version.

`--plugin` may be repeated, accepts `module=/local/dir` for unpublished
modules, and `--no-defaults` drops the standard in-tree plugin set. The resulting
`dist/suxen` is a static binary; copy it into a runtime image of your choice
(the runtime stage of the main `Dockerfile` is a good template).

The SDK image is not an offline builder. `suxen-build` runs the Go module resolver,
which may contact the configured module proxy and checksum service even when a local
plugin imports only Suxen packages. Builds with third-party plugin dependencies also
need those modules. For network-restricted builds, provide a complete private
`GOPROXY`/`GOSUMDB` setup or a pre-populated module and checksum cache, and verify that
configuration in the target build environment.
