# High availability

Every application replica is stateless when it uses PostgreSQL and a shared blob store.
PostgreSQL migrations are serialized with a database advisory lock. Blob publication,
garbage collection, and online store migration share renewable database leases; blob
writes remain immutable and idempotent. Scheduled cleanup uses a renewable leader lease. Do not
share a SQLite file or a `ReadWriteOnce` filesystem volume between replicas.

Proxy cache fills use a database-backed generation for each repository path.
The generation is issued before contacting upstream; a later-started 200, 304,
or 404 that publishes first prevents an older in-flight result from replacing
its cache authority. A later 404 removes the positive row for that cache path
while retaining independent digest aliases. After the negative entry expires,
the next read fetches upstream again. A generation row expires after 24 hours
without a new fetch for its key. Each fetch assesses that expiry when it begins:
an active key keeps its epoch, while an expired key gets a new epoch that
rejects earlier tokens even if cleanup has not reached the row.
These rows are pruned in bounded batches during later proxy fetches, including
rows left by failed requests. Expired negative-cache rows are also pruned in
bounded batches after a one-minute cleanup grace. Warm reads do not run this
cleanup. Expiry bounds inactive cache bookkeeping; it is not a per-request
transfer timeout. A positive fetch whose epoch was retired may use another
authoritative cached result, but cannot publish its own bytes.

The lease for blob publication is per physical store and covers each blob `Put` plus metadata publication.
Writes, garbage collection, and online migration against that store therefore serialize
across all replicas. Extra replicas improve process availability and spread reads and
request preprocessing; they do not multiply write throughput to one store. See the
[capacity baseline and sizing guide](capacity.md).

The blob store may be object storage (`s3://` or `gcs://`) or a filesystem (`fs://`) volume that every
replica mounts. S3 and GCS advertise shared storage automatically. A filesystem store is treated
as node-local by default; to run multiple replicas against a shared `ReadWriteMany` volume,
set `SUXEN_BLOBSTORE_SHARED=true` to assert that the volume is reachable by every replica.
The filesystem driver stages and commits blobs and upload chunks with atomic renames and
externalizes all cross-replica coordination to PostgreSQL, so a backend that provides
atomic same-directory rename and close-to-open consistency — such as CephFS, or NFSv4 with
appropriate mount options — can satisfy the driver contract. The project does not run
CephFS or NFS in its automated topology suite. Setting this flag is the operator's
assertion that the chosen server, client, mount options, and failure behavior provide
those guarantees; validate concurrent publication, replica loss, and paired restore on
that exact deployment before relying on it. Do not set the flag without that evidence.

The automated multi-replica and paired-recovery topology uses PostgreSQL and MinIO. S3
compatibility varies by provider, so run the same acceptance workflow against the selected
service. The shipped GCS driver runs conformance tests against an emulator; those tests do
not prove production Google Cloud credentials, IAM, retention policies, or restore.

## External deployment settings

An external deployment needs these settings on every replica:

```sh
SUXEN_DB='postgres://suxen:password@postgres.example/suxen?sslmode=require'
SUXEN_BLOBSTORE='s3://artifact-bucket/suxen?region=eu-west-1'
SUXEN_CLUSTER='true'
SUXEN_BOOTSTRAP_PASSWORD='replace-with-a-strong-password'
SUXEN_BOOTSTRAP_TOKEN='replace-with-a-long-random-token'
AWS_ACCESS_KEY_ID='...'
AWS_SECRET_ACCESS_KEY='...'
```

S3 credentials come from the standard AWS SDK credential chain, including environment
variables, shared configuration, workload identity, ECS task roles, and EC2 instance
roles. For MinIO, Ceph, or another compatible service, add a percent-encoded `endpoint`
query parameter. `pathStyle` defaults to `true` when an endpoint is present:

```text
s3://artifacts/suxen?region=us-east-1&endpoint=http%3A%2F%2Fminio%3A9000
```

GCS uses application default credentials and a `gcs://bucket/prefix` URL. With Helm,
set `blobStore.gcsCredentialsSecret` to mount a service-account JSON key, or leave it
empty when application default credentials are available through the node environment.

## Helm three-replica values

The Helm chart can create a CloudNativePG cluster and optionally install its operator. The
Object-store URL, credentials, bootstrap credentials, and OIDC state secret should be supplied
through Kubernetes Secrets in production. A minimal three-replica values file is:

```yaml
replicaCount: 3

database:
  bundled:
    enabled: true
    instances: 3

blobStore:
  existingSecret: suxen-storage
  secretKey: blobstore-url
  credentialsSecret: suxen-s3-credentials

bootstrap:
  existingSecret: suxen-bootstrap

config:
  cluster: true
  # Disable the embedded UI routes in the published standard image.
  uiEnabled: false

persistence:
  enabled: false

podDisruptionBudget:
  enabled: true

serviceMonitor:
  enabled: true
  bearerTokenSecret:
    name: suxen-metrics
    key: token

cloudnative-pg:
  enabled: true
```

The `suxen-metrics` Secret must exist in the release namespace and contain a suxen API
token with `admin:stats:read`. The chart references that Secret from the ServiceMonitor; it
does not create the token.

Set `cloudnative-pg.enabled` to `false` when the operator is already installed in the
cluster. For a managed PostgreSQL service, disable `database.bundled` and set
`database.existingSecret` instead. The chart refuses multiple replicas or autoscaling
unless cluster mode, shared PostgreSQL, a shared blob store, and a bootstrap credential
Secret are configured together. The shared blob store is either an S3 or GCS store or the
chart-managed PVC with `blobStore.shared=true` and `persistence.accessModes` including
`ReadWriteMany`.

## Extra OCI listen ports

Hostname routing is cluster-safe: every replica reads `endpoints.hosts` from shared
metadata. Extra listen ports are bound by each process at startup. A create or update
requires every serving replica to restart before the listener is available. Prefer Host
plus Ingress on the primary Service port for HA Docker roots. The
Helm chart derives those extra Ingresses and Service ports from
`provisioning.document` rather than a parallel `extraHosts` / `extraPorts` list.

All values are documented in the [Helm chart guide](../../charts/suxen/README.md); every
configuration variable is in the [configuration reference](../reference/configuration.md).
