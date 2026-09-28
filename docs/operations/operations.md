# Operations guide

This guide covers production ingress, credential rotation, backup and restore, and
upgrades. The [Helm chart guide](../../charts/suxen/README.md) documents topology and
values; the [API guide](../reference/api.md) documents health, readiness, metrics, and
administrative endpoints.

## Ingress and upload limits

Terminate TLS before exposing Suxen outside a trusted network. The proxy must preserve
the request method, path, query string, `Authorization`, `Content-Type`, `Digest`,
`Range`, and OCI upload response headers. Do not rewrite `/v2/`, `/repository/`, or
`/api/` paths.

Set the ingress request-body limit above `config.maxUploadBytes`, and size its request
and response timeouts for the largest intended transfer. Suxen's streaming deadlines
default to unlimited, so a shorter ingress timeout becomes the effective limit. For
ingress-nginx, these controller-specific annotations are a suitable starting point:

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "11g"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "600"
  hosts:
    - host: artifacts.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: artifacts-tls
      hosts: [artifacts.example.com]
```

Equivalent settings have different names in other ingress controllers. Confirm them
with a real upload close to the configured maximum before production use. npm and Cargo
publish requests have an additional 64 MiB parser bound; npm requests are buffered and
need memory for JSON and base64 expansion. The [capacity and availability guide](capacity.md)
defines the tested memory, upload, concurrency, and timeout bounds.

## Credentials and Secret rotation

`SUXEN_BOOTSTRAP_PASSWORD` and `SUXEN_BOOTSTRAP_TOKEN` are first-install inputs. Once
the bootstrap user exists, restarting with changed values does not rotate its password
or token. Create a replacement scoped token through the UI or API, update its consumer,
then delete the old token. Change local user passwords through the user API.

The chart also does not restart pods when an externally managed Secret changes. After
rotating database, S3, OIDC state, metrics, or provisioning Secrets, restart the
Deployment when that value is consumed by Suxen:

```sh
kubectl -n suxen rollout restart deployment/suxen-suxen
kubectl -n suxen rollout status deployment/suxen-suxen
```

A metrics bearer token is consumed by the Prometheus operator through the
ServiceMonitor, not by the Suxen pod. Updating that Secret therefore does not require a
Suxen restart.

## Backup

Back up metadata and blob content as one recovery set. An independent database or blob
backup is not a complete Suxen backup.

For SQLite and filesystem storage:

1. Stop Suxen so uploads, cleanup, and garbage collection are quiesced.
2. Snapshot the complete `SUXEN_DATA` directory, including `suxen.db`, blobs, and upload
   state. With the chart defaults, snapshot the release PVC.
3. Snapshot every named filesystem store whose resolved path is outside `SUXEN_DATA`.
   A copy of `SUXEN_DATA` does not include those paths.
4. Record the running Suxen version, process configuration, named blob-store resources,
   and the environment variables or files referenced by their `configurationRef` fields.
5. Start Suxen and verify `/readyz`.

For PostgreSQL and S3:

1. Stop all Suxen replicas, or otherwise prevent every write, cleanup, and GC request.
2. Take a consistent PostgreSQL backup and a versioned snapshot or copy of every complete
   configured S3 prefix while writes remain quiesced. Include the default store and every
   named store; do not infer their locations from `SUXEN_DATA`.
3. Preserve the database and every object snapshot under one recovery identifier,
   together with the exact bucket/prefix identities, Suxen version, chart values,
   provisioning document, and Secret references.
4. Resume replicas only after both backup parts complete.

S3 versioning and object-lock retention protect against accidental deletion, but they
do not replace the paired metadata backup. Database rows contain password and token hashes,
not the original API tokens or external-store credentials. Preserve the referenced
Kubernetes Secrets, environment sources, workload-identity configuration, and any token
values an automation client must continue using. Test restoration regularly in an
isolated namespace.

## Restore

Restore into an empty database and blob-store location while Suxen is stopped. Restore
both parts of the same recovery set and every named store under its recorded configuration
identity. Restore the configuration and referenced secrets before starting the recorded
Suxen version. Start a single replica first. Wait for migrations and startup provisioning
to finish, then check:

```sh
curl --fail https://artifacts.example.com/healthz
curl --fail https://artifacts.example.com/readyz
```

Pull representative Raw and OCI artifacts and compare their expected SHA-256 digests.
Also verify users and API tokens, repository/blob-store bindings, download and trust
policies, provisioning ownership, and any resumable upload state the recovery point is
expected to retain before scaling out. Keep cleanup disabled during validation if the
restored object snapshot may contain blobs newer than the restored database.

A rollback is the same paired restore operation. Select one recovery identifier and roll
metadata, every blob-store prefix or directory, configuration, and secrets back to that
point together. Never combine a pre-upgrade database with post-upgrade blob snapshots (or
the reverse) and call it a rollback. Extra blobs are normally harmless until GC, but a
database reference to a missing blob makes an artifact unreadable.

### Automated recovery evidence

The automated tests exercise the application-level recovery contract, not a cloud
provider's backup product or an operator's PostgreSQL tooling:

| Topology | Automated recovery scenario | Boundary |
| --- | --- | --- |
| SQLite + filesystem | Stop a bootstrapped server, copy its complete data directory, mutate the source after the recovery point, then start a restored server. The test checks health/readiness, exact Raw and OCI bytes, repository and user state, a scoped API token, and absence of the later write. | Runs in every source check. Named filesystem paths outside `SUXEN_DATA` still require explicit operator snapshots. |
| PostgreSQL + S3-compatible storage | Stop the server, clone its PostgreSQL database and copy all committed blobs to an isolated MinIO prefix, mutate the source, then start the restored server and perform the same acceptance checks. | Runs in the storage-backend job. It validates Suxen against PostgreSQL and MinIO; it does not certify a managed PostgreSQL snapshot service or every S3 implementation. |
| PostgreSQL + shared filesystem | No CephFS or NFS deployment/restore runs in CI. | `SUXEN_BLOBSTORE_SHARED=true` is an operator assertion; rehearse atomic rename, close-to-open visibility, failover, and paired restore on the selected filesystem. |
| PostgreSQL + GCS | Driver contracts run against `fake-gcs-server`. | Production credentials, IAM, retention/versioning, regional behavior, and restore are not established by the emulator. Rehearse them against the selected GCS project before production use. |

## Maintenance task history

Cleanup, garbage collection, verification, and migration record their outcomes in
`/api/v1/tasks`. If a request or scheduler context is canceled, the operation stops
and its final task status is written with a separate five-second timeout. This keeps
cancellation from leaving the task marked as running after the operation returns.

## Verify blob-store integrity

Verify is a read-only background task that cross-checks each blob store against the
digests the metadata references. It mutates nothing.

```sh
# Cross-check every store (no blob content read):
curl --fail -X POST -H "Authorization: Bearer $TOKEN" \
  https://artifacts.example.com/api/v1/verify

# One store, re-hashing present blobs to catch bit-rot:
curl --fail -X POST -H "Authorization: Bearer $TOKEN" \
  "https://artifacts.example.com/api/v1/verify?blobStore=archive&rehash=true"
```

It reports three findings. **Dangling** — a digest the metadata references but the store
no longer holds — is the actionable one: reads of those assets fail. **Orphaned** blobs
are present but unreferenced and are reclaimed by garbage collection. **Mismatched** (only
with `rehash=true`) is content whose recomputed digest no longer matches its key. Each run
records a `blob-store-verify` task readable under `/api/v1/tasks`, publishes the latest
full-run counts to `suxen_blob_verify_findings{kind}`, and logs each dangling or
mismatched blob at warn level.

OCI proxy manifests can refer to layers that have not been downloaded yet.
Verification excludes these pending dependencies until their bytes are published
to a blob store; garbage collection still preserves their retention references.

The scan can overlap publication, garbage collection, and migration. Before reporting
a dangling blob, verification briefly takes the store's operation lease and rechecks
both its current references and physical presence. Scan counters describe observations
during the run, rather than a single transactional snapshot. In particular, orphan
counts can include blobs published after the reference snapshot; garbage collection
independently rechecks references before removing anything.

Re-hashing reads every present, referenced blob, so run it deliberately rather than on a
tight schedule. `SUXEN_VERIFY_INTERVAL` (default `0`, disabled) schedules cross-check-only
runs; re-hash stays a per-request choice.

## Draining a blob store

A blob store's definition — its driver, `configurationRef` location, and physical
destination — is fixed for the store's lifetime; only operational attributes (such as
upload-session limits) can be edited in place. Changing where a store keeps its bytes is
never an in-place edit: create a replacement store and drain onto it, or, for a store that
is still unused, delete it and create it afresh. An attempt to change a stored definition,
through either the API or provisioning, is rejected with a `blob_store_definition_immutable`
conflict. Immutability fixes the *reference*, not the secret behind it: rotating the value
of the referenced environment variable or file still works (restart the process where the
driver reads it only at startup — see [Credentials and Secret rotation](#credentials-and-secret-rotation)).

To retire a blob store, create its replacement, then drain the old store onto it:

```sh
# Mark "archive" for migration onto "archive-v2":
curl --fail -X POST -H "Authorization: Bearer $TOKEN" \
  -d '{"target": "archive-v2"}' \
  https://artifacts.example.com/api/v1/blob-stores/archive/drain
```

While a store is `draining`, no new repository may be bound to it, and no existing
repository may be rebound onto it; repositories already on it keep serving reads. The
target must be a different, existing, active store, and the `default` store cannot be
drained.

The background migrate job (every `SUXEN_MIGRATE_INTERVAL`, default `1m`; `0` disables it)
then copies the draining store's blobs onto the target, rebinds its repositories, deletes
what it has confirmed on the target, and — once the source holds nothing — marks it
`drained`. Publication, rebind, source deletion, and the terminal state share a renewable
database lease. Uploads already staging when the drain begins resolve the binding again
under that lease; while the old binding remains visible they write both source and target.
An acknowledged write therefore remains readable through the rebind. The job is resumable:
an interrupted migration continues on the next tick, and each run records a
`blob-store-migration` task under `/api/v1/tasks`.

If an existing target object has the expected digest key but a different size from
the source, migration fails and keeps the source copy. Repair or remove that target
object with the backend's tools, then rerun migration; the next run copies the
source again. This size check does not verify the bytes of pre-existing target
objects that have the same size.

Draining is reversible. If something goes wrong, clear the drain and keep every blob; the
migrate job stops advancing the store:

```sh
curl --fail -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://artifacts.example.com/api/v1/blob-stores/archive/drain
```

A store's `state` (`active`, `draining`, `drained`) and its `drainTarget` are visible in
`GET /api/v1/blob-stores`. Once a store is `drained` — every repository rebound and every
blob moved — remove it:

```sh
curl --fail -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://artifacts.example.com/api/v1/blob-stores/archive
```

Removal is refused while any repository still references the store.

## Upgrade and rollback

The first public v1 release starts from a new, empty metadata database. Development
database schemas and migration checksums are not a supported upgrade source. Back up any
development database, blob stores, configuration, and secrets together before replacing
the installation; keep that backup with its matching development binary. Do not edit the
migration ledger to make an old database appear current. Re-publish data that must be kept
through the supported APIs into the new installation.

Suxen supports fixes on the latest v1 minor release only. Read the full
[compatibility and support policy](../reference/compatibility.md) before planning an
upgrade.

Database migrations run automatically at startup and are serialized for PostgreSQL.
Before an upgrade, read the release notes, take a complete backup, render the chart
with production values, and upgrade the chart and image together. Stop all old
replicas before starting a new binary that changes the schema. Start one new
replica, wait for migrations and readiness, then start the remaining new replicas.
The migration lock serializes startup migrations; it does not prevent an already
running old binary from writing incompatible data. In particular, migration 0013
normalizes stored timestamps for chronological SQL comparisons, and older writers
must not reintroduce the previous variable-width representation.

Suxen migrations are forward-only. A binary refuses a database containing unknown
newer migrations, so rollback means restoring the pre-upgrade database and blob backup;
installing an older image against an upgraded database is not a supported rollback.

After an upgrade, wait for the rollout and verify readiness, authenticated metrics, a
Raw upload/download, and an OCI push/pull before declaring success:

```sh
kubectl -n suxen rollout status deployment/suxen-suxen
kubectl -n suxen get pods
```
