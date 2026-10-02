# Changelog

All notable changes to Suxen are recorded here. Releases use Semantic Versioning.

## 1.0.0-rc.2

### Features

- Every Raw asset has a component and version, exposed as `raw.component` and
  `raw.version`: its parent directory and file name by default, or the result of
  ordered `formatConfig.components` patterns, which let classification and cleanup
  treat a payload, its side files, or a whole version directory as one version.
- `GET /api/v1/repositories/{name}/components` lists Raw component versions,
  highest first, with their member files; `suxenctl repo components NAME` prints
  them, and `suxenctl repo create` accepts `--component` and `--component-anchor`.
- Cleanup policies accept `order: version` to keep the highest Raw component
  versions or OCI tags instead of the most recently updated entries.
- Format plugins can implement `RetentionGroupingRevision` so the host recomputes
  stored retention groups after their grouping changes.

### Upgrading

- Migration 15 adds the cleanup policy `order` and per-asset component columns. On
  PostgreSQL it also switches `assets.path` to the `C` collation, so path listings
  order bytewise as on SQLite and path-prefix lookups use the path index.
- The first startup after the upgrade computes the new columns for every existing
  asset inside the migration transaction, so its duration grows with the asset
  count. On PostgreSQL, stop replicas running an older version before starting
  the upgraded one: assets they publish afterwards lack the new columns until
  their repository's patterns are next changed.

## 1.0.0-rc.1

First release candidate. This is a prerelease; its API, configuration, persistence,
CLI, chart, and Go extension interfaces may change before `v1.0.0`.

### Features

- Hosted, proxy, and group repositories for Raw, OCI, Maven, Go modules, Cargo, npm,
  and PyPI; read-only Git snapshot proxying.
- SQLite and filesystem for a single node, or PostgreSQL with S3-compatible shared
  storage for multiple replicas. GCS storage is available with the provider limits
  described in the compatibility policy.
- Content-addressed blobs, resumable uploads, cleanup and garbage collection,
  online blob-store migration, and integrity checks.
- Local users, scoped tokens, roles, OIDC browser sessions, private anonymous
  defaults, and per-repository privileges.
- Declarative provisioning, classification, download gates, trust policies,
  signature checks, durable webhooks, tasks, and Prometheus metrics.
- An embedded administration UI, `suxenctl`, Docker images, an OCI Helm chart,
  and public Go plugin interfaces with an SDK image.
- Release archives for Linux, macOS, and Windows on amd64 and arm64, plus
  checksums, SBOMs, signatures, and provenance from the GitHub release workflow.

### Before you install

- Start with an empty database. Development database layouts are unsupported;
  do not rewrite migration checksums to force an upgrade.
- Back up metadata and blob data together. Database migrations run forward only;
  rollback requires restoring the paired backup.
- Supported client protocols, deployment topologies, and verification limits are
  described in the compatibility policy and release test matrix.
