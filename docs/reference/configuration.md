# Configuration reference

suxen is configured entirely through environment variables. This page is the single
source of truth for them; other guides link here rather than repeat the table.

| Variable | Default | Purpose |
| --- | --- | --- |
| `SUXEN_LISTEN` | `127.0.0.1:8080` | HTTP listen address. The runtime image and Helm chart explicitly use `:8080` inside their container network. OCI repositories may bind extra ports on the same host via `endpoints.ports`. |
| `SUXEN_DATA` | `./data` | Data and temporary-upload directory |
| `SUXEN_DB` | `sqlite://<data>/suxen.db` | SQLite URL or PostgreSQL connection URL |
| `SUXEN_BLOBSTORE` | `fs://<data>/blobs` | Blob-store URL for a compiled-in driver (`fs://`, `s3://`, or `gcs://` in the standard build) |
| `SUXEN_BLOBSTORE_SHARED` | `false` | Assert that a filesystem blob store sits on a volume every replica can reach (`ReadWriteMany`), so it can back a cluster deployment without object storage; only for backends with atomic rename and close-to-open consistency (e.g. CephFS) |
| `SUXEN_BOOTSTRAP_USER` | `admin` | First administrator username; follows the local username grammar below |
| `SUXEN_BOOTSTRAP_PASSWORD` | random once | First administrator password; explicit strong value required with Postgres, object storage, or cluster mode |
| `SUXEN_BOOTSTRAP_TOKEN` | random once | First administrator API token; explicit strong value required with Postgres, object storage, or cluster mode |
| `SUXEN_PROVISION` | unset | Absolute local `file://` URL of a desired-state file or directory; invalid desired state stops startup and startup never prunes |
| `SUXEN_CLUSTER` | automatic | Require PostgreSQL and shared blob storage, and gate singleton schedulers with database leader leases |
| `SUXEN_PUBLIC_URL` | request origin | Absolute external HTTP(S) origin used for OIDC callbacks |
| `SUXEN_OIDC_STATE_SECRET` | unset | Shared secret enabling browser OIDC login and OCI Bearer token signing |
| `SUXEN_AUTH_FAILURE_LIMIT` | `10` | Failed credential verifications allowed per direct peer and failure window |
| `SUXEN_AUTH_FAILURE_WINDOW` | `1m` | Interval starting with the first failure; successful authentications do not reset its failure count |
| `SUXEN_AUTH_LOCKOUT` | `5m` | Time a direct peer is denied further credential verification after reaching the limit |
| `SUXEN_AUTH_TRUSTED_PROXY_CIDRS` | unset | Proxy networks whose `X-Forwarded-For` chain may identify authentication clients |
| `SUXEN_CLEANUP_INTERVAL` | `1h` | Enabled-policy scheduler interval; `0` disables scheduled policy cleanup |
| `SUXEN_GC_INTERVAL` | `1h` | Blob garbage-collection and per-replica abandoned-staging cleanup interval; `0` disables both scheduled operations |
| `SUXEN_VERIFY_INTERVAL` | `0` | Blob-store verify interval; `0` (default) disables scheduled verify. Scheduled runs cross-check without re-hashing |
| `SUXEN_MIGRATE_INTERVAL` | `1m` | Interval at which draining blob stores are migrated onto their target; `0` disables the mover |
| `SUXEN_WEBHOOK_RETRY_BASE` | `1s` | Base delay for webhook delivery retries; attempt _n_ waits `base × 2^(n-1)` (capped at 1h) |
| `SUXEN_WEBHOOK_POLL_INTERVAL` | `1s` | How often the delivery worker claims due deliveries; the floor between retry attempts |
| `SUXEN_WEBHOOK_MAX_ATTEMPTS` | `8` | Failed deliveries beyond this count are dead-lettered |
| `SUXEN_PROXY_MANIFEST_TTL` | `5m` | Positive cache TTL for tagged proxy OCI manifests; `0` always revalidates |
| `SUXEN_OUTBOUND_TIMEOUT` | `30s` | Overall OIDC and webhook request timeout |
| `SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT` | `30s` | Maximum wait for proxy upstream response headers; request and response bodies have no overall outbound deadline |
| `SUXEN_OUTBOUND_ALLOWED_CIDRS` | unset | Comma-separated CIDRs allowed to override the private-address egress deny-list |
| `SUXEN_OUTBOUND_ALLOWED_HOSTS` | unset | Comma-separated exact hosts allowed to resolve to otherwise-denied addresses |
| `SUXEN_PROXY_TOKEN_REALM_HOSTS` | unset | Exact cross-host registry token realms authorized to receive upstream Basic credentials |
| `SUXEN_UI_ENABLED` | `true` | Serve the embedded administration UI when the binary contains it |
| `SUXEN_MAX_UPLOAD_BYTES` | `10737418240` | Maximum upload size |
| `SUXEN_MAX_CONCURRENT_UPLOADS` | `4` | Maximum concurrent upload parsers and stagers, including proxy cache fills, per replica |
| `SUXEN_READ_TIMEOUT` | `0` | Overall HTTP request read deadline; `0` permits streaming uploads of any duration, while request headers retain a 10s limit |
| `SUXEN_WRITE_TIMEOUT` | `0` | Overall HTTP response write deadline; `0` permits streaming downloads of any duration |
| `SUXEN_LOG_LEVEL` | `info` | Structured log level |

## Authentication limits

Basic authentication runs at most two concurrent password attempts per peer and
four per process. Each attempt holds its slot through local password verification
and any configured OIDC password grant. When those slots are full, up to 16
requests per peer and 64 per process wait for a slot for at most 15 seconds.
Waiting ends when the client disconnects. Requests that exceed the queue limits
or wait limit receive the usual unauthorized response without checking the
password or adding a failed attempt to the peer's failure count. The failure
budget and lockout are checked again before a waiting request starts. The peer
is the socket peer's IP address unless `SUXEN_AUTH_TRUSTED_PROXY_CIDRS` permits
a trusted forwarding chain.
These concurrency limits are fixed; the failure limit, window, and lockout above
control the separate history of completed failures.

Cold OIDC provider discovery shares one outbound request among concurrent callers
for the same provider configuration. At most four discoveries run per process, with
up to eight callers waiting per provider; excess bearer attempts fail authentication.
A caller that disconnects stops waiting without canceling discovery for other callers.
Each shared discovery has the `SUXEN_OUTBOUND_TIMEOUT` deadline (30s by default),
and a failed discovery can be retried by a later request.

## Bootstrap credentials and cluster mode

Local usernames must start with an ASCII letter or digit and may then contain
ASCII letters, digits, `.`, `_`, `@`, `+`, or `-`. Uppercase names and email-style
names are valid. This applies to API creation, provisioning, and the bootstrap
administrator; slashes and colons are rejected because they conflict with user
URLs and HTTP Basic authentication.

PostgreSQL, S3, and `SUXEN_CLUSTER=true` deployments enable cleanup lease gating and fail
startup unless both bootstrap credentials are explicitly configured. Passwords must
contain at least 12 characters, tokens at least 24, and placeholder values such as
`change-me` are rejected. SQLite with filesystem storage keeps the secure first-run
behavior: omitted credentials are generated and logged once. Explicitly configured
credentials are never echoed back to the log.

Explicit `SUXEN_CLUSTER=true` additionally requires PostgreSQL and a shared blob store
together — S3 or GCS object storage, or a filesystem store with
`SUXEN_BLOBSTORE_SHARED=true`. Setting
`SUXEN_CLUSTER=false` with either distributed backend is rejected, while omitting it enables
leases automatically from the configured backend.

`SUXEN_PUBLIC_URL`, when set, must be an `http` or `https` origin without credentials, a
path, query, or fragment. If OIDC providers exist without `SUXEN_OIDC_STATE_SECRET`,
startup warns that interactive browser login is disabled; OIDC Bearer authentication
continues to work. The same secret signs short-lived OCI Distribution tokens at
`/v2/token`. Without it, `GET /v2/` advertises only Basic and anonymous `docker pull`
cannot complete the token handshake.

During initialization, startup keeps `/healthz` available and `/readyz` unavailable
while retrying temporary database failures or provisioned blob-store readiness
failures with capped backoff. Invalid configuration, an incompatible database
migration history, invalid provisioning input, and an occupied extra OCI listener
port stop the process with an error.

## Transport and secrets

Run suxen behind a TLS reverse proxy outside trusted development networks. Local passwords
use Argon2id, API tokens are stored only as SHA-256 hashes, and every blob is verified
against its digest before it becomes visible.

## Outbound egress control

Proxy, webhook, and OIDC requests deny loopback, private, link-local, unspecified, and
multicast addresses by default. The same check is applied after DNS resolution and on
every redirect; connections are made to the checked address so DNS cannot change the
destination between validation and dialing. Redirects do not retain authorization,
cookies, proxy credentials, referrers, or webhook signatures. Redirects originating from
POST or another unsafe method are rejected so request bodies cannot be replayed to a
different service.

For a legitimate internal service, prefer the narrowest exception. Allow an exact DNS name
with `SUXEN_OUTBOUND_ALLOWED_HOSTS=identity.internal`, or a limited network range with
`SUXEN_OUTBOUND_ALLOWED_CIDRS=10.20.30.0/24`. Entries are comma-separated, exact, and do
not accept wildcards. An outbound host exception permits network access but does not permit
registry credential forwarding. If a proxy registry uses a token realm on a different host
and that realm requires the registry's configured Basic credentials, also list it in
`SUXEN_PROXY_TOKEN_REALM_HOSTS`. Cross-host token requests are otherwise made without those
credentials.
