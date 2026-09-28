# Black-box interoperability baseline

This tier treats suxen as an external service. It deliberately does not import Go
packages or synthesize OCI/cosign payloads in-process. Bats drives operator-facing
clients against this topology:

```text
Docker / ORAS / Crane / Cosign / curl
                     |
                  nginx
                     |
             suxen A / B / C
                |         |
           PostgreSQL    MinIO

Zot OCI 1.1 --- OCI proxy       credentialed HTTP ---- Raw proxy
HMAC sink  <---- webhook delivery and retry history
```

`compose.e2e.yaml` publishes every host-facing port dynamically on `127.0.0.1` and the
runner chooses a unique Compose project name. There are no global container names or
fixed host ports or host bind mounts, so concurrent CI jobs and job-container runners
with a sibling Docker daemon do not collide. Startup provisioning creates the shared
resources once under a database lease. Health checks and `compose up --wait` replace
fixed startup sleeps; the only deliberate long wait is the 30-second leader lease
expiry exercised by failover.

## Running

The CI job calls `install-ci-tools.sh` and then:

```sh
SUXEN_E2E_FULL=1 make test-interop
```

For local full-suite runs, use Java 21 (the version used in CI) and set
`JAVA_HOME` so `java`, Maven, and Gradle all use it. The pinned Gradle 8.14.3
cannot run on Java 25. Install Bats plus the clients needed by a scenario. Without
`SUXEN_E2E_FULL=1`, scenarios requiring Docker registry clients or Cosign are skipped,
while curl/Compose topology scenarios still run. In full mode, a missing required client
is a failure rather than a skipped assertion. The runner removes its uniquely named
containers, networks, volumes, temporary credentials, runtime file, and project-local
built images. It intentionally retains shared pulled-image layers in Docker's cache.
Local runs publish dynamic ports on loopback. When the runner detects that clients are
inside a CI job container controlling the host Docker daemon, it publishes on the
daemon host and gives HTTP, ORAS, Crane, and Cosign the job container's
bridge-gateway address. Docker commands retain a separate host-loopback registry address
so the daemon continues to treat the ephemeral HTTP registry as local and insecure.
An unfiltered full-mode run asserts that no scenario was skipped. Missing tools and
newly skipped supported scenarios therefore fail the CI tier.

The client installer pins Bats 1.12.0, ORAS 1.2.2, Cosign 2.4.1, Crane 0.20.3,
Gradle 8.14.3, Maven 3.9.11, and Prometheus/promtool 3.2.1. Published
checksums are verified for every binary release that provides them. The Compose
topology digest-pins the Postgres image and builds MinIO and its client from
[pinned official source](../support/minio/README.md), since upstream no longer
serves their public container images or binary downloads;
remaining support images use exact patch or dated release tags so Renovate can update
them visibly. Maven and Gradle use the Java runtime supplied by the runner; the full
tier fails explicitly when `java` is unavailable.

## Scenario map

| File | Assertions |
| --- | --- |
| `00_bootstrap` | three ready replicas, idempotent provisioning, version/OpenAPI |
| `01_oci_hosted` | Docker across replicas, manifest HEAD, upload ceiling, multi-platform Crane selection |
| `02_oci_proxy` | OCI 1.1 pull-through, positive/negative cache, signed Bearer authentication, read-only proxy |
| `03_oci_group` | tag and shared-subject referrer union, member fallback, read-only group |
| `04_raw_hosted` | content-addressed S3 dedup and prefix listing |
| `05_raw_proxy` | credentialed upstream, cache hit count, response redaction, group |
| `06_rbac_anon` | HTTP-created role/user/token, denied delete, immediate revoke, safe whoami output |
| `07_cleanup_gc` | classification, keepLast preview, last-download predicate, S3 GC |
| `08_webhooks` | receiver-side HMAC verification for delete, eight-attempt dead letter |
| `09_provenance_trust` | real Cosign keys, OCI 1.1 referrers, fingerprint denylist and policy invalidation |
| `10_download_gates` | release and re-quarantine for OCI manifests |
| `11_cluster_pg_s3` | cross-replica IO, failover, dedup, leader takeover, migration race |
| `12_observability` | health, dependency-aware readiness, metrics, build version |
| `13_maven_clients` | Maven and Gradle hosted publication, proxy/group publish rejection, and hosted/proxy/group resolution |

The former strict-mode gaps are mandatory scenarios: the suite uses the pinned Crane
to select both children of a multi-platform index, a local RSA token issuer with a real
Zot OCI 1.1 verifier, ORAS referrers attached to a byte-identical subject in hosted and
proxy members, and Cosign's explicit `oci-1-1` registry referrer mode. The suite also
sends a multi-request Docker push through nginx, verifying that OCI upload-session
state can move between replicas through the shared blob-store backend.

Documented single-node client workflows live in `examples/*/test.bats`. In particular,
the OCI example owns basic ORAS and Helm round-trips, the Raw example owns byte-preserving
upload/download and the development profile's explicit anonymous read-only access, and the
download-gate example owns the Raw
release/re-quarantine workflow. The E2E suite does not repeat those assertions.

On a failure the runner prints the final 300 log lines for every service. To inspect a
topology manually, copy the Compose invocation from `run.sh` with a chosen project name:

```sh
docker compose --project-name suxen-e2e-debug \
  --file test/e2e/compose.e2e.yaml up --build --wait
```

Remove that manual topology with the same project and file arguments plus
`--profile migration-race down --volumes --remove-orphans`.
