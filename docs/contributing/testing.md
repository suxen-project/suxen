# Testing

suxen has three test tiers: fast source checks, executable examples for documented
single-node workflows, and a black-box interoperability suite for behavior that depends
on a multi-replica topology or dedicated protocol fixtures.

## Source checks

```sh
make test
make check
make build
```

`make check` runs Go formatting and vet (including the plugin build-tag combinations), the
race-enabled Go tests, the dependency-free administration-UI tests, and `make check-example`,
which builds the out-of-tree example plugin against the tree. `make test`
verifies both the standard and `noui` build variants. The administration UI in the
standard build has no JavaScript build-time or runtime dependencies and exercises the same
documented REST resources as the CLI.

Storage contracts are documented at their interfaces, and the Go tests exercise Raw
hosted/proxy/group behavior, OCI push/pull behavior, signed webhook delivery, and
attribute-gated downloads. Provenance tests cover signed push and pull enforcement, cosign
OCI referrers, SLSA DSSE envelopes, certificate identities, fingerprint denials, and
trust-policy invalidation.

Recovery tests stop a bootstrapped server and restore a paired metadata/blob recovery
point through a new server. SQLite/filesystem runs in the ordinary source suite;
PostgreSQL/MinIO runs in the storage-backend job. Both check Raw and OCI bytes, scoped
token access, control-plane state, and rollback exclusion of a post-snapshot write. The
MinIO and GCS-emulator jobs do not certify provider backup tooling, cloud IAM, or a
CephFS/NFS deployment; those remain operator acceptance tests documented in the
[operations guide](../operations/operations.md).

## Executable example tier

Every documented example has a Bats test that boots its own native Suxen process and
drives the commands shown to users. These tests own single-node client round-trips and
operator workflows, including Raw byte preservation, explicitly enabled anonymous
read-only access, Raw
download-gate release/re-quarantine, and ORAS and Helm OCI publication.

```sh
make test-examples
```

Public CI rejects skipped example scenarios. Keeping these checks with the example makes
the documentation executable and avoids repeating the same assertion in the larger
PostgreSQL/MinIO topology.

## Black-box interoperability tier

The gated end-to-end baseline runs real Docker, ORAS, Crane, Cosign, Maven, Gradle, curl,
and Prometheus clients against three suxen replicas behind nginx. The replicas share
PostgreSQL and MinIO and interact with a real Distribution registry, a credentialed Raw
upstream, and an HMAC-verifying webhook receiver. The suite covers hosted, proxy, and group
data planes; authorization and token revocation; cleanup and blob collection; webhook
retry history; trust and quarantine enforcement; replica failover and migration races; and
operational endpoints. It includes multi-platform image selection, an authenticated
registry:2 Bearer-token handshake, shared-subject referrer union, Cosign OCI 1.1
referrers, and Maven/Gradle publication and resolution across hosted, proxy, and group
repositories.

Run the complete tier with its pinned client versions installed:

```sh
SUXEN_E2E_FULL=1 make test-interop
```

The runner uses a unique Compose project name, dynamically published loopback ports, and a
temporary Docker credential directory. It always tears down containers, networks, and
volumes, and prints service logs when a scenario fails. A lightweight local run without
`SUXEN_E2E_FULL=1` still checks the HTTP/topology scenarios and reports real-client
scenarios as skipped. The full tier requires every scenario to run and rejects any skip.
See [test/e2e/README.md](../../test/e2e/README.md) for the scenario map and troubleshooting
commands.

An assertion stays in this tier when it exercises a property the single-node examples
cannot establish: load-balanced upload sessions, cross-replica visibility, shared object
storage, PostgreSQL coordination, failover, dependency outages, authenticated upstream
handshakes, proxy/group aggregation, retry/dead-letter behavior, or protocol-specific
limits. Some client setup necessarily overlaps an example when it is the prerequisite for
one of those assertions. For example, the E2E Docker push is retained because requests in
one upload session cross replicas, and Maven publication is retained so the same run can
verify hosted/proxy/group resolution and write rejection. Basic ORAS, Helm, Raw, explicitly
enabled anonymous access, and Raw-gate round-trips are owned only by the examples.

The maintained feature-to-test mapping is in
[release-test-matrix.md](release-test-matrix.md).

Resumable OCI upload sessions live in the repository's configured blob store, so each
request in a multi-request push may land on a different replica. The suite exercises that
path through the load balancer and verifies the resulting PostgreSQL metadata and S3
content from another replica.

## When to run the interoperability tier

Run it when changing a data-plane protocol, authentication, ingress behavior, blob
storage, or clustered request flow. The [contributing guide](../../CONTRIBUTING.md) lists
the full pull-request expectations.
