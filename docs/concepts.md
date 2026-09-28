# Concepts

This page explains the ideas you meet when using suxen. For the internal design and seams,
see the [architecture guide](contributing/architecture.md).

## Content addressing

Artifact bytes are stored as immutable blobs keyed by their SHA-256 digest. Identical
uploads are stored once per blob store (deduplication), every write is verified against
its digest before it becomes visible, and nothing is ever mutated in place. This is the
property that makes caching, garbage collection, and multi-node operation simple.

## Repositories: formats and types

A repository has a **format** and a **type**. `raw`, `oci`, `maven`, and `go` support
every type; the other plugin formats support a subset (see below).

Formats determine the wire protocol and coordinate parsing: `raw` (arbitrary files
addressed by path) and `oci` (the OCI Distribution API, under `/repository/{name}/v2/`
or a bound host/port at `/v2/`) are built in. Additional formats are supplied by
plugins, and the standard build ships several: `maven`, `go`, `cargo`, `npm`, and
`pypi` (hosted, proxy, group), and `git` snapshots (proxy). Creating an unsupported
(format × type) combination is rejected.

Types determine where content comes from:

- **Hosted** — authoritative storage you push to.
- **Proxy** — a pull-through cache of a single upstream, with negative caching and
  per-proxy upstream authentication.
- **Group** — a read-only, ordered union of members of the same format.

See the [repositories guide](guides/repositories.md) to create and manage them.

## Assets, components, and attributes

- An **asset** is a named file in a repository (a Raw path, an OCI layer or manifest). It
  points at exactly one blob and carries access metadata.
- A **component** is a derived grouping of assets that share an identity or version (an
  image `name:tag`). It is not a stored type.
- An **attribute** is namespaced key/value metadata attached to an asset (and, for policy
  projection, rolled up to the logical component), written by suxen's own verifiers or by
  external services. Attributes are strongly consistent and are the shared input the
  policy engine reads.

## The policy model

suxen's policy subsystems share one model: **attributes are the blackboard; classifiers
and verifiers write it; predicates read it.**

- **Classification** stores a label derived from ordered rules over a coordinate.
- **Cleanup policies** are scheduled typed predicates that select artifacts for deletion.
- **Download gates** are read-time predicates that withhold an asset until a condition
  holds — the basis of upload-then-quarantine-until-scanned workflows.
- **Provenance** verification writes its verdict into the same attribute space.

The guides for [classification and cleanup](guides/classification-cleanup.md),
[webhooks and gates](guides/webhooks-gates.md), and [provenance](guides/provenance.md)
cover each in practice.

## Identity and access

Access follows **privilege → role → subject**: privileges name an action on a scope, roles
bundle privileges and may nest, and subjects are local users or OIDC identities. Anonymous
access is itself a configurable role. See the [authorization guide](guides/authorization.md).

## Deployment topologies

suxen runs as a single node (SQLite plus a filesystem blob store) or as a stateless
application tier over PostgreSQL and an S3-compatible object store — the same binary, only
configuration differs. Multi-replica behavior (concurrent startup, cross-replica reads,
replica loss, and scheduler-leader takeover) is covered by a real PostgreSQL and MinIO
integration test. See [high availability](operations/high-availability.md).
