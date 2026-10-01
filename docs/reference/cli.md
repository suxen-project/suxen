# `suxenctl` reference

`suxenctl` administers a running Suxen server and transfers Raw artifacts. `suxenctl
version` prints the client build version as text. Commands that return resource data
write JSON to standard output; `raw get` writes to its destination file, and deletes
normally produce no output. Diagnostics go to standard error. A non-zero exit status
means that argument validation, transport, authentication, authorization, or the
server operation failed.

## Connection and authentication

The global flags must precede the command:

```text
suxenctl [--url URL] [--token TOKEN] COMMAND
```

`--url` defaults to `SUXEN_URL`, then `http://localhost:8080`. `--token` defaults to
`SUXEN_TOKEN` and is sent as a Bearer token. Prefer a scoped API token over a local
administrator token for automation.

Redirects must retain the configured URL's scheme, hostname, and effective port.
The client rejects redirects to another origin before forwarding credentials or
request bodies, including HTTPS-to-HTTP redirects.

Use `whoami` to inspect the current identity and its effective privileges:

```sh
suxenctl whoami
```

The API may identify an unauthenticated request as `anonymous`. See
[the API guide](api.md) for authentication, privilege, pagination, and error
contracts.

## JSON input files

Administration commands that take a JSON `FILE` require exactly one value, followed only
by whitespace. Numeric values are forwarded without rounding. Resource create
and update commands expect the matching object from the OpenAPI schema. `attribute set`
expects a JSON object containing the complete replacement value for one namespace.
Raw transfer `FILE` arguments are artifact bytes or destination paths; signature and
certificate files also contain bytes rather than JSON.

The server rejects writes to reserved, system-owned attribute namespaces such as
`sys`, `raw`, `oci`, `maven`, `go`, `cargo`, `npm`, `pypi`, `git`, `classification`,
and `provenance`. Plugin-projected coordinate namespaces are also read-only.

## Desired state

```text
suxenctl apply -f FILE [--dry-run] [--prune]
```

`apply` resolves environment and file secret references locally before sending the
desired state. It requires HTTPS except when connecting to a loopback address.
`--dry-run` reports changes without applying them. `--prune` deletes eligible managed
resources omitted from the document; built-in repositories and roles are protected.
See [the provisioning guide](../operations/provisioning.md).

## Repositories and storage

```text
suxenctl blob-store list
suxenctl blob-store get NAME
suxenctl blob-store create FILE
suxenctl blob-store update NAME FILE
suxenctl blob-store delete NAME

suxenctl repo list
suxenctl repo create [--format FORMAT] [--type hosted|proxy|group]
                     [--blob-store NAME] [--upstream URL]
                     [--members NAME,...] [--hosts HOST,...]
                     [--ports PORT,...] [--allow-overwrite=true|false]
                     [--format-config JSON]
                     [--component PATTERN [--component-anchor PATTERN]]... NAME
suxenctl repo assets NAME [PREFIX]
suxenctl repo components NAME
suxenctl repo delete NAME
```

`--component` adds one Raw component pattern to `formatConfig.components`, in
command-line order; `--component-anchor` sets the anchor of the preceding
`--component`. It cannot be combined with components given in `--format-config`.
`repo components` prints the component listing; for a Raw repository with
component patterns it prints one entry per version with its member files:

```sh
./bin/suxenctl repo create \
  --component '^(?P<name>models/.+)/(?P<version>[0-9][^/]*)/[^/]+$' \
  --component-anchor '\.glb$' \
  models
./bin/suxenctl repo components models
```

`--allow-overwrite` controls replacement in hosted repositories. Omit it to use the
format default, or pass `--allow-overwrite=false` to reject different content at an
existing artifact path or OCI tag. Identical retries remain allowed.

Collection commands follow all server cursors and print one JSON array. Repository
formats are registry-driven; use a format name compiled into the server. The standard
build includes Raw and OCI plus the Maven, Go, Cargo, npm, PyPI, and git format
plugins. `--hosts` and `--ports` bind extra OCI
registry roots; they are rejected on non-OCI repositories.

Declaratively managed resources reject imperative updates and deletes unless ownership
is explicitly transferred through the HTTP API's documented `force=true` contract.

## Raw artifacts

```text
suxenctl raw put [--signature FILE] [--certificate FILE] REPOSITORY PATH FILE
suxenctl raw get REPOSITORY PATH FILE
suxenctl raw delete REPOSITORY PATH
```

The optional signature and certificate flags attach provenance material to an upload.
`PATH` uses literal characters: the client escapes spaces, `?`, `#`, `%`, and Unicode
per segment while retaining `/` separators. It rejects leading, trailing, or
repeated slashes, `.` and `..` segments, backslashes, and C0/DEL control characters. See the
[Raw API path contract](api.md#raw-artifact-api).

`raw get` creates parent directories for the destination file when necessary. It
stages a private file in that directory (mode `0600` on Unix), syncs it, and replaces the
destination only after the complete transfer. Failed or canceled transfers keep an
existing destination and remove the temporary file. Connect, TLS handshake, and
response header waits are bounded; large response bodies have no overall timeout.
Ctrl-C or SIGTERM cancels an active request.

## Asset attributes and verification

```text
suxenctl attribute get REPOSITORY ASSET_ID NAMESPACE
suxenctl attribute set --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE FILE
suxenctl attribute delete --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE
suxenctl verify REPOSITORY ASSET_ID FILE
```

`attribute set` replaces the complete namespace object; it does not merge individual
fields. Attribute mutations require the digest returned by `repo assets`; the server
rejects the mutation if the asset path was replaced after that digest was observed.
`verify` runs the repository's configured trust verification using the request in `FILE`.

## Identity and access

```text
suxenctl role list
suxenctl role create [--description TEXT] [--privileges VALUE,...] NAME
suxenctl role delete NAME

suxenctl user list
suxenctl user create --password PASSWORD [--roles NAME,...] NAME
suxenctl user roles [--set NAME,...|--clear] NAME
suxenctl user token [--name NAME] [--scopes VALUE,...] NAME
suxenctl user tokens NAME
suxenctl user revoke NAME TOKEN_ID

suxenctl oidc-provider list
suxenctl oidc-provider get NAME
suxenctl oidc-provider create FILE
suxenctl oidc-provider update NAME FILE
suxenctl oidc-provider delete NAME
```

OIDC provider secrets are write-only and are not returned by list or get operations.
An update may omit `clientSecret` to retain the existing secret.

## Classification, policy, and cleanup

```text
suxenctl classification get REPOSITORY
suxenctl classification set REPOSITORY FILE
suxenctl classification defaults get
suxenctl classification defaults set FILE
suxenctl classification defaults delete

suxenctl cleanup-policy list
suxenctl cleanup-policy create FILE
suxenctl cleanup-policy update NAME FILE
suxenctl cleanup-policy delete NAME
suxenctl cleanup [--apply] REPOSITORY POLICY

suxenctl download-gate get REPOSITORY
suxenctl download-gate set REPOSITORY FILE
suxenctl download-gate delete REPOSITORY
suxenctl download-gate defaults get
suxenctl download-gate defaults set FILE
suxenctl download-gate defaults delete

suxenctl trust-policy get REPOSITORY
suxenctl trust-policy set REPOSITORY FILE
suxenctl trust-policy delete REPOSITORY
suxenctl trust-policy defaults get
suxenctl trust-policy defaults set FILE
suxenctl trust-policy defaults delete
```

Cleanup is a preview unless `--apply` is present. Download gates and cleanup policies
use the shared typed predicate model documented in [the API guide](api.md).
A cleanup policy FILE may set `"order": "version"` to keep the highest component
versions or OCI tags; an omitted `order` means `updatedAt`.

## Webhooks

```text
suxenctl webhook list
suxenctl webhook get NAME
suxenctl webhook create FILE
suxenctl webhook apply NAME FILE
suxenctl webhook update NAME FILE
suxenctl webhook delete NAME
suxenctl webhook deliveries [--limit N] NAME
```

`apply` performs a named PUT and therefore creates or replaces the webhook. `update`
uses the same current PUT contract and remains available for explicit CRUD workflows.

## Operations

```text
suxenctl task list [--limit N]
suxenctl task get ID
suxenctl task leader
suxenctl stats
suxenctl gc [--apply] [--grace DURATION]
```

Garbage collection is a preview unless `--apply` is present. `--grace` defaults to
`24h` and protects recently unreferenced content from racing active writes. Scheduled
cleanup selection and physical blob collection are independent workers; see
[the operations guide](../operations/operations.md).

Run `suxenctl help` for the compact command synopsis bundled with the binary.
