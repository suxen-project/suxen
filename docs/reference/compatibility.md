# Compatibility and support policy

This policy defines the v1 contract for the control plane, configuration, CLI,
persistence, and public plugins. Compatibility promises apply from `v1.0.0`.
Prerelease builds may introduce breaking changes.

## Supported release window

After v1 is published, fixes are made on the latest published v1 minor release. Patch
releases within that minor may contain bug, security, documentation, and dependency
fixes. There is no extended-support branch or backport promise for earlier minors.

Operators should test each update against a restored copy of their metadata and blob
backup before production rollout. Release notes and [CHANGELOG.md](../../CHANGELOG.md)
describe known migration or behavior changes.

## v1 contract inventory

The following sources define the public v1 boundary. An
implementation detail outside this table does not become public merely because it is
exported from an internal package or appears in a response log. The validation named
here is summarized more precisely in the
[release test matrix](../contributing/release-test-matrix.md); it does not turn an
unrehearsed deployment combination into a supported one.

| Surface | Authoritative contract | Compatibility check |
| --- | --- | --- |
| Control-plane HTTP API and JSON | the served [`openapi.json`](../../internal/server/openapi.json) under `/api/v1` | route/OpenAPI bidirectional contract tests, schema fixtures, and real `suxenctl` calls |
| Raw and OCI data planes | [API reference](api.md), plus OCI Distribution behavior for `/v2` | server protocol tests and the zero-skip real-client suite |
| Process configuration | [configuration reference](configuration.md) | configuration parser tests and clean-install examples |
| Provisioning document | [`suxen.io/v1` provisioning guide](../operations/provisioning.md) and its documented resource kinds | strict document/engine tests and executable provisioning examples |
| Privilege grammar and private anonymous default | [authorization guide](../guides/authorization.md) | privilege parser, built-in defaults, route authorization, RBAC, and explicit anonymous-access tests |
| `suxen` and `suxenctl` commands and JSON | [`suxenctl` reference](cli.md) and `suxen [serve\|version]` | command tests, executable examples, and release binary version checks |
| SQLite and PostgreSQL persistence | the initial schema files in `internal/store/migrations` | immutable checksum and final-shape tests; server-level paired restore tests for SQLite/filesystem and PostgreSQL/MinIO |
| Helm values and rendered resources | [`charts/suxen/values.yaml`](../../charts/suxen/values.yaml) and the [chart guide](../../charts/suxen/README.md) | lint, representative renders, and chart/image version checks |
| Public Go extension APIs | packages below [`spi/`](../../spi) and the [extension guide](../contributing/extending.md) | registry/conformance tests, separate-module blob/format/API consumers, and SDK-image compilation and boot of the custom distribution |

The inventory covers implemented public contracts. Event-handler, middleware, and
scheduled-task plugin APIs are outside this inventory. Additive fields, operations,
provisioning kinds, privileges, chart values, and SPI helpers follow the compatibility
rules below.

## Contract surface versions

Several public surfaces carry their own SemVer. For `v1.0.0-rc.1`, all report
`1.0.0-rc.1`; they may advance independently after the stable v1 boundary is
established. [`internal/contract/versions.yaml`](../../internal/contract/versions.yaml)
is the source of truth; a running server also reports the matrix under
`versions[].version` and the `contract` array of `GET /api/v1`.

| Surface | Contract | Audience | A major means |
| --- | --- | --- | --- |
| `blobstore-spi` | `spi/blob` exported API | blob-driver authors (compile time) | a type or method signature is removed, changed, or made mandatory |
| `format-spi` | `spi/format` exported API | format authors (compile time) | as above |
| `plugin-api-spi` | `spi/api` exported API | control-plane route authors (compile time) | as above |
| `http-api` | served `openapi.json` under `/api/v1` | HTTP clients | an operation or field is removed or repurposed (a new path prefix) |
| `webhook-api` | delivery payload and `X-Suxen-*` headers | webhook receivers | a payload field or header is removed, renamed, or retyped |

Within a surface's major, a minor adds an optional interface, operation, field, enum
value, or header, and a patch is a documentation or behavior fix with an unchanged shape.
Prerelease surfaces may change before `v1.0.0`; no contract version locks are
enforced for this candidate. The stable release will establish baselines for
subsequent surface version changes. The compatibility promise to users begins
when `v1.0.0` is published.

## Control-plane API

`/api/v1` and its OpenAPI document are the control-plane compatibility boundary.
Within v1:

- existing operations and fields are not removed or repurposed;
- new optional operations, fields, enum values, and problem codes may be added;
- clients must ignore response fields they do not understand;
- breaking changes require a new API version.

Every registered `/api/v1` route and method is checked bidirectionally against the
OpenAPI document in CI. The same contract tests verify request objects, response
schemas, pagination, privilege annotations, problem details, and local references.

Raw and OCI repository endpoints are separate data-plane contracts. OCI routes follow
the Distribution protocol; Raw routes retain their documented HTTP semantics. A
format plugin may define an additional client protocol and documents its own supported
surface.

The bundled Cargo, npm, and PyPI hosted protocols return a generic server
message for unexpected internal failures. Their documented publication
conflict, policy, and upload-limit responses remain specific. Git smart HTTP
returns a generic `ERR` packet or side-band error for unexpected snapshot
failures; malformed commands and unsupported protocol usage remain specific.
Internal causes are recorded in request-correlated server logs.

The standard binary includes GCS storage plus Maven, Go modules, Cargo,
npm, PyPI, and Git format plugins. Maven, Go modules, Cargo, npm, and PyPI provide
hosted, proxy, and group repositories. Git is proxy-only,
serving depth-one snapshots rather than a general Git host. The checked-in public CI
workflow is configured to require the executable example and native-client
interoperability matrices to complete without skipped scenarios before a tag is
released.

## Configuration and command line

Environment variable names and provisioning kinds are compatibility-sensitive. New
options may be added with defaults. Removing or changing an existing option requires a
documented migration and a deprecation period.

The `suxenctl` command grammar follows singular resource nouns. Scripts should consume
JSON output rather than human diagnostics and must tolerate additive response fields.

## Persistence and rollback

Database migrations are forward-only. A newer binary may migrate metadata at startup;
an older binary refuses a database with unknown newer migrations. Rollback therefore
means restoring the metadata and blob data captured before the upgrade, not pointing an
older binary at an upgraded database.

Metadata and blob content form one recovery set. Follow the
[backup and restore procedure](../operations/operations.md) and verify `/readyz`, Raw transfers,
and OCI push/pull after restoration.

## Deployment topology

The automated server deployment scenarios run on Linux. Windows and macOS
archives are provided for evaluation; they do not establish supported server
deployments on those operating systems. The configured native Windows/amd64
job exercises filesystem blob and upload-session round trips plus generic staging
ownership and reclamation. Neither
the packaged Windows server/CLI nor the macOS archives have a native execution
gate.

The supported single-node topology is SQLite with a filesystem blob store. The
supported multi-replica topology is PostgreSQL with a blob driver that declares shared
storage capability. A filesystem store is not shared merely because multiple pods can
mount the same path. The canonical automated topology is PostgreSQL with MinIO-backed S3;
other S3 services require provider acceptance testing. Shared-filesystem safety is an
explicit operator assertion and has no CephFS/NFS CI deployment. GCS conformance uses an
emulator and does not establish production IAM, credential, retention, or restore behavior.
The [operations guide](../operations/operations.md) records the exact automated recovery
checks and provider-specific acceptance responsibilities.

Automated deployment coverage also exercises PostgreSQL with MinIO in a three-replica
topology. Recovery coverage starts restored single servers over paired SQLite/filesystem
and PostgreSQL/MinIO recovery points. Real-cloud credentials and IAM, provider-native
backup tooling, and a shared-filesystem multi-replica deployment require separate
rehearsals.

Release charts and standard UI-capable images are versioned and tested together. Keep
the chart's default application version or set an explicit matching image tag. The
project does not publish a separate `noui` image; that build tag is available for custom
local binaries and images. The matching `suxen-sdk` image is the supported builder for
custom plugin distributions. Native processes bind loopback by default; the runtime image
and Helm chart explicitly bind the container network. The standard image contains the
administration UI and accepts the bootstrap/API token directly in its sign-in form, so
OIDC is optional for a single-node installation.

Go packages under `internal/` are not public APIs. Public plugin compatibility is
limited to the contracts under `spi/` and is documented in
[the extension guide](../contributing/extending.md). Within v1, an interface
implemented by an external plugin will not gain mandatory methods; such a change
is breaking even though rebuilding would expose the compile error. Additive
capabilities use new optional interfaces. Compile-time plugins run as trusted
code in the server process and are not a security isolation boundary.
