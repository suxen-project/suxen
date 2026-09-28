# Troubleshooting guide

Start with the smallest observation that separates process health, dependency health,
authentication, and authorization:

```sh
curl --fail --silent "${SUXEN_URL}/healthz"
curl --fail --silent "${SUXEN_URL}/readyz" | jq
suxenctl whoami
```

`/healthz` proves the process can answer HTTP. `/readyz` checks the metadata database
only; blob-store availability is deliberately excluded, so a store outage does not drop
a replica from rotation (it surfaces per request instead — see below). A healthy process
with failing readiness therefore points at the database and should remain out of service
until that error is resolved.

## HTTP failures

Control-plane failures use RFC 7807 JSON with a stable `code`. OCI routes use the OCI
Distribution error envelope. Record the status, response body, request path, request
method, and the matching structured server log entry before retrying.

| Symptom | First checks |
|---|---|
| `401 unauthorized` | Credential presence and type; token revocation; OIDC issuer, audience, and expiry; authentication lockout metrics |
| `403 forbidden` | `suxenctl whoami`; the operation's OpenAPI `x-suxen-required-privilege`; token scopes; the anonymous role |
| `404 not_found` | Resource spelling, repository visibility, and whether the parent repository exists |
| `409 conflict` | Provisioning ownership, resource references, stale pagination cursor, or immutable/in-use blob-store configuration |
| `413` | Ingress body limit and `SUXEN_MAX_UPLOAD_BYTES`; OCI upload-session size |
| `429` | Authentication throttling or staged-upload quota; wait for the reported condition rather than blind rapid retries. A `TOOMANYREQUESTS` that persists across pushes means abandoned upload sessions fill the principal's quota until the next garbage-collection run reaps them; run `POST /api/v1/gc?dryRun=false` to reap immediately |
| `500`/`503` | `/readyz`, database pool state, blob operation errors, disk capacity, and dependency logs |

Do not log authorization headers, cookies, OIDC tokens, blob-store URLs containing
credentials, or provisioning Secret values when collecting diagnostics.

## Database and readiness failures

For SQLite, verify that the `SUXEN_DATA` volume is mounted, writable, and has free space
and inodes. Only one replica may use SQLite. For PostgreSQL, verify DNS, TLS and
credentials, connection limits, and network policy from the Suxen pod. The metrics
`suxen_database_pool_open_connections`, `..._in_use_connections`, and
`..._idle_connections` show pool pressure.

Migrations are serialized and run at startup. An unknown newer migration means an older
binary was started against a newer database; restore the matching binary or the
pre-upgrade backup. Do not edit `schema_migrations` manually.

During initialization, `/healthz` remains available and `/readyz` returns unavailable
while temporary database or provisioned blob-store readiness failures are retried with
capped backoff. Check the structured `startup initialization failed; retrying` log
entry and the dependency's health. If the process exits, inspect its final error
for invalid configuration, incompatible schema history, invalid provisioning, or a
listener bind conflict.

## Blob storage and disk pressure

Blob errors are counted by `suxen_blob_operations_total{operation,result}`. A blob-store
outage does not fail `/readyz` (readiness gates on the database only), so it shows up as
per-request `500`/`503` responses and a rising `result="error"` count rather than a
replica dropping out of rotation. Errors on every replica at once usually indicate shared
S3/GCS credentials, endpoint, or network policy; errors on one replica point instead to
pod-specific Secret mounts, DNS, or ephemeral storage.

Uploads use temporary space before immutable content is committed. Filesystem-backed
deployments need headroom for staged and final content; object-store drivers may spool
through pod ephemeral storage. Check filesystem bytes and inodes, pod ephemeral-storage
limits, and the ingress timeout before treating a large upload as a repository bug.
Configure `attributes.uploadSessions` on space-constrained blob stores to shorten stale
session retention and cap aggregate/per-principal staging. Sessions idle for longer than
`staleAfter` are removed, together with their staged object, by each garbage-collection
run; the task result reports them as `staleUploads` and `staleUploadsDeleted`.
If an OCI upload cannot start after staging begins, Suxen cleans up the staged object
before releasing its quota-counting session. A backend cleanup failure leaves the
session discoverable for the stale-session reaper.

Generic uploads and proxy cache fills also stage files under `SUXEN_DATA/uploads`.
Each live request holds an advisory lock on its file. After a process crash or
restart, files older than one hour with no lock are reclaimed by local scheduled
maintenance on each replica or by a global GC request. Global GC reports these as
`staleStagingFiles` and `staleStagingFilesDeleted`; a dry run only counts them.
The one-hour minimum protects files while they are first being created, even
when the blob GC grace period is set to zero.

Cleanup removes metadata first. Physical reclamation is performed separately by blob
garbage collection after its grace period. A successful cleanup therefore does not
immediately reduce object-store usage. Track `suxen_cleanup_deletions_total` and
`suxen_gc_deleted_blobs_total`, and inspect the corresponding task result before using
manual GC with a shorter grace period.

## Scheduled work in a cluster

Exactly one replica should hold each singleton role. Inspect:

```text
suxen_leader{role="cleanup_scheduler"}
suxen_leader{role="gc_scheduler"}
suxen_leader{role="provisioning"}
suxen_leader_last_held_timestamp_seconds{role="..."}
```

Across all ready replicas, one current holder is expected while a role is active.
Multiple holders indicate database/lease misconfiguration; no recent holder indicates
disabled scheduling, database errors, or an interval longer than the observation
window. Provisioning also runs at startup and fails the process when desired state is
invalid—read the complete preflight report rather than applying resources individually.

## Authentication and OIDC

Compare `suxen_authentication_failures_total` with
`suxen_authentication_blocked_total`. A sharp rise in both can be an attack or clients
reusing an expired Secret. Behind an ingress, configure
`SUXEN_AUTH_TRUSTED_PROXY_CIDRS` only for proxies that sanitize `X-Forwarded-For`; an
incorrect setting can group unrelated clients under one throttle source or trust a
spoofed address.

For OIDC, verify the configured issuer exactly matches the token `iss`, the token
audience contains the client ID, the signing key appears in the issuer JWKS, and pod
time is synchronized. Interactive login additionally requires `SUXEN_PUBLIC_URL` to
match the browser-visible origin and a shared `SUXEN_OIDC_STATE_SECRET` on every
replica.

## Proxy repositories and webhooks

For a proxy miss, check upstream DNS/TLS, outbound allowlists, token-realm allowlists,
and upstream credentials. `suxen_proxy_cache_requests_total` distinguishes hit from
miss by repository and format. Never add a broad outbound CIDR merely to suppress one
failed request; allow the narrow endpoint and token realm actually required.

Webhook queue depth and dead letters are exposed as `suxen_webhook_queue` and
`suxen_webhook_dead_letters`. Delivery outcomes appear in
`suxen_webhook_deliveries_total`. Verify receiver DNS/TLS, outbound policy, HMAC Secret,
and response latency. Use the delivery API to inspect attempts; retries are bounded, so
fix the receiver before replaying a dead letter.

## Metrics triage

`/metrics` requires a bearer token with `admin:stats:read`. Useful initial alerts are:

- readiness unavailable for several minutes;
- any sustained rate of `suxen_http_errors_total` or blob operation errors;
- any `suxen_blob_operations_total{result="not_found"}` on `get`/`head`: the metadata
  references a blob the store no longer holds (a dangling reference). Each occurrence
  is also logged at warn level as `referenced blob missing from store` with the
  repository, blob store, digest, and asset path;
- nonzero `suxen_blob_verify_findings{kind="dangling"}` from the last verify run: the
  metadata references blobs the store no longer holds. Run `POST /api/v1/verify` to
  refresh the counts and list the affected digests in the task result;
- rising authentication blocked rate;
- nonzero webhook dead letters;
- aggregate refresh failures;
- database connections near the configured database limit;
- no recent leader holder for an enabled singleton role;
- sustained growth in `suxen_blob_bytes` without expected repository growth.

Repository labels are deliberately bounded. Repositories beyond the metric cardinality
limit are reported as `repository="overflow"`; use logs and the administration API for
per-repository detail in that case.

When escalating an incident, include the Suxen version, deployment topology, redacted
configuration, failing request envelope, relevant structured logs, readiness response,
and a short metrics window. Include neither artifacts nor credentials unless the
recipient and transfer mechanism are explicitly approved.
