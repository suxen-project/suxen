# Changelog

All notable changes to Suxen are recorded here. Releases use Semantic Versioning.

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
