# API guide

Suxen exposes three HTTP surfaces:

| Surface | Base path | Contract |
| --- | --- | --- |
| Control plane | `/api/v1` | OpenAPI 3 document at `/api/openapi.json` |
| Raw artifacts | `/repository/{repository}/{path}` | Simple HTTP upload/download API |
| OCI artifacts | `/v2` and `/repository/{repository}/v2` | OCI Distribution protocol |

Operational probes are exposed separately at `/healthz`, `/readyz`, `/metrics`, and
`/version`.

## Authentication

The control plane accepts:

- HTTP Basic authentication for local users;
- `Authorization: Bearer <token>` for Suxen API tokens or configured OIDC ID
  tokens;
- the HTTP-only OIDC browser-session cookie created by the login flow.

API tokens are returned only once when created. The token list contains metadata, not
the secret. Prefer a scoped token for automation:

```sh
export SUXEN_URL=https://artifacts.example.com
export SUXEN_TOKEN=replace-with-a-scoped-token

curl --fail-with-body \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  "${SUXEN_URL}/api/v1/whoami"
```

A request to an operation not granted to the anonymous role receives `401`.
Control-plane requests do not receive a `WWW-Authenticate` challenge, so browsers
can use the OIDC login flow without a native Basic-auth dialog; registry requests
retain their client-compatible authentication challenges. Public discovery and
repositories granted to `anonymous` remain accessible without credentials. An
authenticated principal without the required privilege receives `403`.
Administrator accounts bypass role lookup, but token scopes still restrict them.

Mutating requests that use the OIDC browser-session cookie require an `Origin` header
matching `SUXEN_PUBLIC_URL`, or the request origin when no public URL is configured.
Failures return `403` with code `csrf_validation_failed`. Browser `fetch` supplies this
header for same-origin mutations. Basic and Bearer clients are unaffected, even when a
browser also sends an unrelated session cookie. Deployments behind an ingress whose
external origin differs from the upstream request host must configure
`SUXEN_PUBLIC_URL`.

Credential verification is throttled per server replica and direct socket peer. By
default, ten failed Basic, API-token, OIDC-bearer, or OIDC-session verifications within
one minute lock that source out for five minutes. Requests during the lockout receive
the same authentication failure as invalid credentials. Successful verification does
not reset the peer's failure count within that window. `X-Forwarded-For` is ignored
unless the direct peer belongs to `SUXEN_AUTH_TRUSTED_PROXY_CIDRS`; Suxen then walks
the chain from the trusted edge toward the first untrusted client address. Configure
only proxy networks that overwrite or safely append that header. Without a trusted-proxy
setting, clients behind an ingress share that replica's direct-peer limit. Tune
`SUXEN_AUTH_FAILURE_LIMIT`, `SUXEN_AUTH_FAILURE_WINDOW`, and `SUXEN_AUTH_LOCKOUT` for
the topology, and alert on `suxen_authentication_failures_total` and
`suxen_authentication_blocked_total`.

Repository operations use
`repository:<name>:<read|write|delete|annotate|manage>`. Control-plane resources use
`admin:<resource>:<read|write|run>`. The resource segment matches the first path
segment after `/api/v1` (`admin:gc:run` for `POST /api/v1/gc`). Plugin routes
use `admin:plugin-{pluginID}:read|write`, separate from core resource grants.
Grants may replace a segment with
`*`, and a final wildcard matches the remaining suffix. The effective catalog and
current principal are available from:

```text
GET /api/v1/privileges
GET /api/v1/whoami
```

Asset reads/deletes, attribute updates, verification, and cleanup execution use the
repository privilege for the repository in the path. Browse and search filter their
results to readable repositories. Other `/api/v1` resources use their corresponding
`admin:<resource>:...` privilege.

`annotate` permits asset attribute changes and verification. Changing repository
classification, download gates, or trust policies requires `manage`; scanners
should not receive this grant. Group downloads enforce the supplying member's
download gate and trust policy, so those policies cannot be configured on a group.

Attribute `PUT` and `DELETE` requests must carry `If-Match` with the asset's current
strong digest ETag (returned by the attribute `GET`). A comma-separated list or
repeated `If-Match` lines may include other strong ETags, but one must match the
current digest. A single bare digest remains accepted for older clients; wildcard
and weak ETags do not authorize annotation writes. A missing precondition returns
`428`; a nonmatching precondition returns `412`. Scanner results are therefore
applied only to the bytes they inspected.

## OpenAPI contract

The running server serves the exact contract for its build:

```sh
curl --fail --silent "${SUXEN_URL}/api/openapi.json" >suxen-openapi.json
```

Clients can discover that contract, compiled-in formats, plugin identifiers,
and the stable data-plane mounts without authentication:

```sh
curl --fail --silent "${SUXEN_URL}/api/v1" | jq
```

Plugin routes live under `/api/v1/plugins/{pluginID}/`. The served `/api/openapi.json`
includes them for the plugins compiled into that binary — the host fills in the
required privilege, path parameters, and shared error responses; the plugin
supplies the operation and explicit success response for every method — so the
downloaded contract includes the routes compiled into the build that served it.
Plugin authors describe request and response body schemas inline. `GET /api/v1`
lists the identifiers and its `mounts.pluginRoutes` value gives their path;
privileges follow `admin:plugin-{pluginID}:read|write`.

The document defines every core `/api/v1` method in both directions: CI fails if
a route is undocumented or a documented operation is not registered. It also defines
request/response schemas, security alternatives, status codes, pagination parameters,
and RFC 7807 errors. Every operation carries `x-suxen-required-privilege`, whose value
is the exact required privilege or one of the documented `public` and
`repository:<visible>:read` discovery markers. OCI and Raw data-plane routes are
intentionally outside this control-plane document.

JSON request objects reject unknown fields. Item names and IDs in a URL are
authoritative for `PUT` operations; clients do not need to duplicate those identifiers
in request bodies unless the schema requires them.

Omitted-field behavior is specific to each resource; clients must follow the
operation's schema and rules below rather than assume that every `PUT` preserves
or clears omitted values.

Repository `PUT` replaces mutable settings: omitted `formatConfig`, `members`,
and `endpoints` reset those fields. `format` and `type` must be supplied and cannot
change. There is one secret-preservation exception: omit `upstream` to retain the
complete stored URL, including credentials and query parameters redacted by `GET`.
An explicit upstream URL replaces URL credentials but must retain the existing
endpoint (including its query). Do not send a redacted `GET` URL back unchanged.

Raw hosted and proxy repositories accept `formatConfig.components`, the ordered
component patterns described in
[Raw components](../guides/repositories.md#raw-components). Invalid patterns return
HTTP 400 with `invalid_format_config`. For such a repository,
`GET /api/v1/repositories/{name}/components` returns one row per component version,
with every member file in `assets`, using the same `limit` and `cursor` parameters and
`repository:{name}:read` privilege as other repositories; unmatched paths are not
listed. Cleanup policies accept `order`
(`updatedAt` or `version`); an omitted `order` is stored as `updatedAt`, including
on `PUT`.

User updates preserve omitted fields; an empty or omitted password leaves the
existing password unchanged. Creating a user requires a nonempty password.
Omitting `admin` preserves its current value within the update transaction, including
concurrent administrator changes. The dedicated user-roles `PUT` requires a non-null
`roles` array: `[]` clears assignments, while a missing or null field returns `400`
without changing them. Role lists in responses are always arrays, including `[]`.
Usernames must match `[A-Za-z0-9][A-Za-z0-9._@+-]*` and contain at most 255 ASCII bytes. Invalid usernames, missing
creation passwords, and the reserved repository name `default` return `400` with
codes `invalid_username`, `password_required`, and `reserved_repository_name`.

Repository and instance-wide download-gate `PUT` operations require a non-null
`criteria` array. Missing or null criteria return `400` without changing the gate or
its provisioning ownership. An explicit `[]` clears that gate's criteria; inherited
criteria still apply unless inheritance is disabled.

Deleting an asset by ID targets that stored generation. If another upload replaces
it before deletion, the delete returns `404` and preserves the replacement and its
companion metadata.

Stored artifact downloads evaluate `If-Match` / `If-Unmodified-Since` and
`If-None-Match` / `If-Modified-Since` before byte ranges. A stale, weak, or date-based
`If-Range` validator causes a full `200` response instead of a partial response;
use the digest ETag for safe resumptions across rapid replacements. Authorization
and download policies are enforced even when a validator would otherwise produce
`304 Not Modified`.

These validators and requests for a single byte range also apply to the asset-ID
download endpoint. Its OpenAPI operation declares the request headers and the
`206`, bodyless `304` and `412`, and `416` responses, including range headers.

## Collections and cursors

Control-plane collections return a bounded envelope:

```json
{
  "items": [],
  "nextCursor": "opaque-value",
  "total": 128
}
```

`limit` defaults to 100 and accepts 1 through 200. Omit `cursor` for the first request,
then pass `nextCursor` unchanged:

```sh
curl --fail-with-body \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  "${SUXEN_URL}/api/v1/repositories?limit=50"
```

Snapshot-offset cursors are bound to the route, filters, and collection snapshot.
`invalid_cursor` means a value is malformed. `stale_cursor` means it belongs to
another collection or its consistency check failed; restart without a cursor.
The keyset routes below use their own documented consistency rules.

Snapshot-offset collections also report `total`, the full result count, and accept a
1-based `page` parameter for random access (`?page=3&limit=50`) — the basis for the
UI's numbered pagination. `page` and `cursor` cannot be supplied together. A page
does not carry the snapshot check, so a page past the current end returns empty
`items`. History collections that paginate by id (task history, webhook deliveries)
omit `total` and do not accept `page`.

Discovery routes (`/api/v1/browse`, `/api/v1/repositories/{name}/browse`,
`/api/v1/repositories/{name}/components`, and `/api/v1/search`) use cursor-only
pages without `total` or random-access `page`. Repository browsing uses name
order; artifact browse/search use repository, group-member, then path order.
A discovery response examines at most 1,000 underlying asset records. Cross-
repository search also examines at most 1,000 top-level repository records per
response (plus the members of groups reached in that page),
so selective filters may return empty `items` with a
`nextCursor`; continue until the cursor is absent. Artifact cursors exclude assets
inserted after the first request. Existing asset updates may appear, and deletion
of a cursor anchor or a changed group/visibility set can require restarting.
Repository discovery and cross-repository search walk repository names live:
newly created names after the current cursor may appear, while names before it
are skipped. If a repository changes while search is within it, the cursor can
become stale and require restarting.

`GET /api/v1/repositories/{name}/components` returns component-version rows in
asset-path order. Each row names its component (image name for OCI) and one
version. It accepts `limit=1..200` and an opaque `cursor`; `nextCursor` continues
the traversal, and one component can span pages. It does not report `total` or
accept numbered `page` requests. For OCI it lists manifests only; layer blobs
are reached by opening a manifest. An empty page can still have `nextCursor`
when the bounded scan skipped only blobs or shadowed group assets; follow it to
finish traversal. `GET /api/v1/repositories/{name}/assets/{id}/manifest` reads
one OCI-manifest asset's config, layers, child manifests, and subject (with per-
descriptor media types and sizes), parsed from the stored manifest body.
For a group repository, the component row's `assetId` can be opened through
`GET /api/v1/repositories/{group}/assets/{id}` and its `/manifest` endpoint.
The ID resolves only while that member asset is visible through the group's
ordered members; a shadowed or unrelated asset returns `404`. Group asset
details and attributes are read-only. To modify an asset, use its member
repository directly.

`GET /api/v1/repositories/{name}/assets/{id}/download` serves the stored
asset selected by its ID, with the same download gate and trust policy as a
repository-path download. Group IDs must still be visible through the group's
ordered members. Use this route for cached proxy assets whose `path` is an
opaque storage key; their original URL may have included a query that is not
retained in asset metadata.

Repository asset listing accepts a case-sensitive path `prefix` and uses `cursor`,
not `page` or `total`. Non-group repositories read bounded SQL pages in path order. Groups scan
their ordered members in path order, showing the first member's asset for each
path. For npm groups, `prefix` uses the client-facing `formatPath` when a proxy
tarball has an opaque cache `path`. npm, Cargo, and PyPI artifacts are visible
only when their stored member index selects that member and their cache identity
matches the currently advertised artifact. Superseded generations and assets
shadowed by another member are hidden from group listing and asset-ID download.
Historical identities remain separate assets in the direct proxy repository;
an earlier identity becomes visible through the group again if its index
advertises that identity again. Inventory reads use stored metadata and do not
refresh upstreams; a repository-path download may refresh expired metadata
before selecting its member.
A group page examines at most 1,000 underlying records, so it may have
empty `items` with a `nextCursor` when records are shadowed; follow the cursor
to finish traversal. Group responses name the group as the repository, and
the returned IDs can be opened through its asset detail route. The cursor fixes
the highest asset ID at the first request, excluding newly inserted or recreated
paths. Existing asset updates may appear with new values; deleting the cursor
anchor returns `409 stale_cursor` and requires a restart. A changed group member
set also makes the cursor stale. Cross-repository search accepts `q`,
`pathPrefix`, `classification`, and repeated exact projected-attribute filters.
Each `attribute` value is limited to 1,024 bytes, with at most 20 filters per
request. JSON numeric values match by exact decimal value, including large exponents:

```sh
curl --get --fail-with-body \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  --data-urlencode 'pathPrefix=releases/' \
  --data-urlencode 'attribute=sys.blobStore=constrained' \
  "${SUXEN_URL}/api/v1/search"
```

## Errors

Control-plane errors use `application/problem+json` and include a stable lowercase
`code` extension:

```json
{
  "type": "urn:suxen:problem:not_found",
  "title": "Not found",
  "status": 404,
  "detail": "resource not found",
  "code": "not_found"
}
```

Clients should branch on HTTP status and `code`, not the human-readable `detail`.
Internal failures return a generic response; the correlated server log contains the
cause and the same `X-Request-ID`. A valid caller-provided request ID is echoed, and the
server generates one when it is missing or invalid.

OCI failures instead use the Distribution `errors` envelope required by registry
clients. A provenance evaluation that completes but rejects evidence returns its typed
result with status `422`; it is not an internal server failure.

## Common control-plane operations

The OpenAPI document is authoritative for the complete schema. Common resource groups
are:

| Resource | Operations |
| --- | --- |
| Blob stores | `/api/v1/blob-stores`, `/api/v1/blob-stores/{name}` |
| Repositories | `/api/v1/repositories`, `/api/v1/repositories/{name}` |
| Assets and attributes | `/api/v1/repositories/{name}/assets...` |
| Browse and search | `/api/v1/browse`, `/api/v1/repositories/{name}/browse`, `/api/v1/repositories/{name}/components`, `/api/v1/search` |
| OCI manifest contents | `/api/v1/repositories/{name}/assets/{id}/manifest` |
| Users, tokens, and roles | `/api/v1/users...`, `/api/v1/roles...`, `/api/v1/privileges` |
| OIDC providers | `/api/v1/oidc-providers`, `/api/v1/oidc-providers/{name}` |
| Retention | `/api/v1/cleanup-policies...`, `/api/v1/repositories/{name}/cleanup` |
| Trust and quarantine | repository `classification`, `trust-policy`, `download-gate`, and `verification` routes; instance-wide `/api/v1/classification-defaults`, `/api/v1/trust-policy-defaults`, and `/api/v1/download-gate-defaults` |
| Webhooks | `/api/v1/webhooks...` including per-name delivery history |
| Operations | `/api/v1/stats`, `/api/v1/tasks...`, `/api/v1/gc` |
| Desired state | `/api/v1/provision` |

`POST /api/v1/provision` can return HTTP 200 with individual resources marked
`status: "failed"`. REST clients must inspect every `results[].status`; a 200
response alone does not mean the apply succeeded. `suxenctl apply` prints the
full report to stdout and exits nonzero when any result failed, including with
`--dry-run`. Invalid desired state returns a 400 problem. Store and driver
failures return a generic 503 problem, or a redacted failed resource in a 200
report; the server log records the cause under the response's `X-Request-ID`.
Independent desired resources continue after a per-resource failure so the report
lists all outcomes. Pruning is skipped if any desired resource fails, preserving
dependencies for a later retry. A `status: "skipped"` prune result means the
resource's ownership changed after the prune plan was made, so it was left in place.

Declaratively managed resources expose `managed: true`. Configuration mutations are
rejected with `409 managed_resource`; `force=true` removes current declarative
ownership for that mutation. To keep imperative control, remove the resource from the
desired document before the next apply. `GET /api/v1/provision` lists current
declarative ownership.

### Resource naming convention

Public HTTP and UI names use lowercase kebab case, CLI nouns are singular, SQL tables
use plural snake case, and provisioning kinds retain Go-style lower camel case. The
repository singleton category is deliberately closed to classification, download gate,
and trust policy.
Resource names are limited to 255 ASCII bytes.

Trust policies can be set only on hosted and proxy repositories. Setting one on a group
returns `400 invalid_trust_policy`; group downloads enforce the supplying member's
policy, including its inherited instance default. Changing a repository with a trust
policy into a group also returns `400 invalid_trust_policy`; remove the policy first.

| Go type | SQL table | API path | OpenAPI schema | Provision kind | CLI noun | UI key |
| --- | --- | --- | --- | --- | --- | --- |
| `BlobStore` | `blob_stores` | `/api/v1/blob-stores/{name}` | `BlobStore` | `blobStore` | `blob-store` | `blob-stores` |
| `CleanupPolicy` | `cleanup_policies` | `/api/v1/cleanup-policies/{name}` | `CleanupPolicy` | `cleanupPolicy` | `cleanup-policy` | `cleanup-policies` |
| `OIDCProvider` | `oidc_providers` | `/api/v1/oidc-providers/{name}` | `OIDCProvider` | `oidcProvider` | `oidc-provider` | `oidc-providers` |
| `Classification` | `classification_rules` | `/api/v1/repositories/{name}/classification` | `Classification` | `classification` | `classification` | `classification` |
| `DownloadGate` | `download_gates` | `/api/v1/repositories/{name}/download-gate` | `DownloadGate` | `downloadGate` | `download-gate` | `download-gate` |
| `TrustPolicy` | `trust_policies` | `/api/v1/repositories/{name}/trust-policy` | `TrustPolicy` | `trustPolicy` | `trust-policy` | `trust-policy` |

Create a hosted Raw repository:

```sh
curl --fail-with-body \
  --request POST \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  --header 'Content-Type: application/json' \
  --data '{"name":"releases","format":"raw","type":"hosted","blobStore":"default"}' \
  "${SUXEN_URL}/api/v1/repositories"
```

Cleanup and garbage collection are previews unless explicitly applied. Preserve this
two-step workflow in automated tooling:

```text
POST /api/v1/repositories/{name}/cleanup?policy={policy}&dryRun=true
POST /api/v1/repositories/{name}/cleanup?policy={policy}&dryRun=false
POST /api/v1/gc?dryRun=true&grace=24h
POST /api/v1/gc?dryRun=false&grace=24h
```

`POST /api/v1/gc` requires `admin:gc:run`. Task history stays under `/api/v1/tasks`.

Declarative reconciliation uses `POST /api/v1/provision`; see
[the provisioning guide](../operations/provisioning.md) for canonical documents, pruning, and secret
handling.

## Raw artifact API

Raw repositories use standard HTTP methods. Their path contract is:

Stored asset paths must be valid UTF-8 and are limited to 2048 bytes across
SQLite and PostgreSQL. Invalid paths return HTTP 400 with `invalid_path`.

The asset path is a sequence of nonempty segments separated by single `/` characters.
Leading, trailing, and repeated `/` characters, `.` and `..` segments, backslashes,
and C0/DEL control characters are rejected with `400`; paths are never silently normalized. A
literal space, `?`, `#`, `%`, or Unicode character belongs to its segment and must
be URL escaped on the wire. Escape each segment separately so intended `/`
separators remain. The server decodes the URL once and preserves the resulting
asset path. A successful PUT returns a `Location` URL with the same escaping;
clients can use it directly for GET and DELETE. For example,
`release notes/100% café?.txt` is
`release%20notes/100%25%20caf%C3%A9%3F.txt` in a URL. A Raw request looks like:

```sh
curl --fail-with-body \
  --request PUT \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  --upload-file artifact.tar.gz \
  "${SUXEN_URL}/repository/releases/project/artifact.tar.gz"

curl --fail-with-body \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  --output artifact.tar.gz \
  "${SUXEN_URL}/repository/releases/project/artifact.tar.gz"

curl --fail-with-body \
  --request DELETE \
  --header "Authorization: Bearer ${SUXEN_TOKEN}" \
  "${SUXEN_URL}/repository/releases/project/artifact.tar.gz"
```

Hosted repositories expose the `allowOverwrite` setting through repository creation
and update. Omitted updates preserve its stored value. A denied replacement returns
409; an identical-content retry succeeds. See the
[repository guide](../guides/repositories.md#replacing-existing-assets) for format
defaults and the distinction between artifacts and mutable indexes.

Uploads may include `Digest: sha256:<lowercase-hex>` to require an expected digest and
`X-Suxen-Signature` for verify-on-push policies. Proxy and group repositories are
read-only.

Raw, Maven, and Go uploads return `413` when the upload exceeds the size limit
and `400` when the request body cannot be read. Local staging storage failures
return a generic `500`; filesystem details are recorded in server logs.

## OCI Distribution API

The default `oci` repository is mounted at `/v2` on the primary listen port, so ordinary
registry clients use names such as `artifacts.example.com/team/image:tag`. Other OCI
repositories can claim the same `/v2` root by hostname (`endpoints.hosts`) or extra listen
port (`endpoints.ports`); hostname matches win over port matches. A specifically named OCI
repository is also available under `/repository/{repository}/v2` for clients that can
configure a path prefix.

Suxen implements authenticated discovery, monolithic and resumable blob uploads,
manifest and blob access, tag listing with `n`/`last` pagination, deletion, OCI 1.1
referrers (including `artifactType` filtering), `GET /v2/_catalog`, cross-repository
blob mount within the same suxen repository, `PATCH` `Content-Range` checks, and
`Accept-Ranges` / HTTP 206 on blob GET. Hosted manifest PUTs require one JSON
document with `schemaVersion: 2`; missing or other schema versions and trailing JSON
return `MANIFEST_INVALID`. Subject-bearing manifests require a canonical SHA-256
subject digest; successful pushes return `OCI-Subject`. Referrer descriptors
preserve manifest annotations and derive `artifactType` from `config.mediaType`
when the manifest omits its top-level `artifactType`. Malformed referrer digest
requests return `DIGEST_INVALID`.
Catalog and tag-list `n` defaults to 100 and
rejects values above 1000. Proxy catalog, tag, and referrer reads follow upstream
`Link: rel="next"` continuations only on the configured upstream endpoint, up to
32 pages; a continuation beyond that limit fails instead of returning a partial
listing. `GET /v2/` challenges unauthenticated clients
with Bearer (when `SUXEN_OIDC_STATE_SECRET` is set) and Basic so Docker can obtain a
short-lived pull token or send stored credentials on later uploads. Authenticated ping
still requires the target repository's `read` privilege. Artifact GET/HEAD follow that
same read grant, so anonymous ORAS, Crane, curl, and `docker pull` succeed only after the
operator grants repository read to `anonymous`; Docker also requires token signing.
Extra listen ports bound by
`endpoints.ports` serve only `/v2`, `/healthz`, and `/readyz`; they do not expose the
control plane. The listener set is fixed at process startup. Creating or updating a port
binding persists it, but every serving replica must restart before the new listener is
available. Startup stops if a configured port cannot be bound.

After an interrupted resumable upload, query the upload session's `Location` with
`GET` and resume from the returned `Range`. Suxen reconciles stored bytes with the
upload ledger before reporting a recovered offset or accepting another append.
For a `PATCH` or closing `PUT` with `Content-Range: start-end`, the range is
inclusive: the request body must contain exactly `end - start + 1` bytes, and
`start` must equal the current upload offset. This check also applies when the
request uses chunked transfer encoding and has no declared `Content-Length`.
A mismatched range returns `416` without appending bytes; query the session
before retrying from its current offset.
If another operation still holds the session lease, retry after the `429`
response's `Retry-After` delay. Recovered bytes count toward staging quotas, and
current limits still apply to further appends and completion.
An interrupted operation can retain its reserved staging capacity until the
session is retried, canceled, or removed by stale-session cleanup, even if it
stopped before writing bytes.

`GET`/`POST /v2/token` (and `/repository/{name}/v2/token`) issue HMAC-signed registry
tokens scoped to the actions the caller already has. Empty scope defaults to `pull`.
`docker login` is still required to push. Direct HTTP, ORAS, and other clients can use
anonymous artifact routes whenever the anonymous role allows them.

Registry tokens recheck local accounts' administrator status on each request,
including accounts named `anonymous`. Anonymous handshakes use only anonymous
role privileges, independently of any same-named local account. Older ambiguous
tokens for a local account named `anonymous` require a fresh login/token exchange
after upgrading to this build.

## Operational endpoints

| Endpoint | Authentication | Meaning |
| --- | --- | --- |
| `/healthz` | none | Process liveness only |
| `/readyz` | none | Metadata (database) readiness; blob stores are not checked |
| `/version` | none | Running build version |
| `/metrics` | bearer token with `admin:stats:read` | Prometheus/OpenMetrics exposition |

Health, readiness, and metrics access logs are suppressed to avoid probe noise. Their
failures and request metrics remain observable.
All operational endpoints accept only `GET` and `HEAD`. Readiness gates only on the
metadata database, the one dependency every request path needs; blob-store availability
is deliberately not checked, because every replica shares the same external stores and
failing readiness on a store blip would drop all replicas at once. Store failures
surface per request instead. Anonymous readiness failures name the unavailable
dependency class, not configured resources. `/version`
is the deliberate public build-disclosure endpoint; other responses do not include a
global version header.

## Compatibility

Additive fields and operations remain compatible within `/api/v1`. Removing or changing
an operation or field requires a new API version. Clients should tolerate unknown
response fields, use the served OpenAPI document for code generation, and treat cursors
as opaque.
