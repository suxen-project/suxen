# Repositories

A repository is a named endpoint with a `format` (`raw` or `oci` in the core, plus the
`maven`, `go`, `cargo`, `npm`, `pypi`, and `git` plugin formats in the standard build),
a `type` (`hosted`, `proxy`, or `group`), and a bound blob store. The default
installation creates a `raw` and an `oci` hosted repository on first start.

## Listing and inspecting

```sh
./bin/suxenctl repo list
./bin/suxenctl repo assets raw source/
```

## Creating repositories

A hosted repository stores what you push to it; a proxy caches an upstream; a group
presents an ordered union of members. `raw`, `oci`, `maven`, `go`, `cargo`, `npm`,
and `pypi` support every type; `git` is proxy-only. Creating an unsupported
combination is rejected.

```sh
./bin/suxenctl repo create \
  --format raw \
  --type proxy \
  --upstream https://downloads.example.com \
  upstream-cache

./bin/suxenctl repo create \
  --format raw \
  --type group \
  --members raw,upstream-cache \
  public
```

A proxy fetches an artifact from its upstream on the first miss and serves the
content-addressed cached copy afterward. Missing paths are negatively cached for one
minute. Tagged OCI manifests are revalidated after `SUXEN_PROXY_MANIFEST_TTL`, while
digest-pinned manifests and blobs remain immutable cache hits. OCI proxies understand
Registry Bearer challenges and token services, while basic upstream credentials can be
supplied as URL user information. API responses preserve only the upstream origin and
path; user information, query parameters, and fragments are redacted because signed or
query-authenticated upstream URLs can carry credentials.
An upstream partial response cannot fill a proxy cache entry; Suxen returns an
upstream error and retries the full fetch on the next request.

Suxen does not serve stale mutable metadata when revalidation fails. During an upstream
outage, warm immutable blobs and digest-pinned manifests remain available, but an expired
package index, mutable metadata file, or tagged OCI manifest returns the upstream error.
See [capacity and availability](../operations/capacity.md) for this boundary.

A group searches its members in order and is read-only. OCI groups merge tags and
referrers across hosted and proxy members. npm, Cargo, and PyPI groups select an artifact's member from
the current package indexes, so an expired member index must revalidate before the
download can proceed. Members must be existing hosted or proxy
repositories with the group's format; groups cannot contain other groups. A repository
referenced by a group cannot be deleted until that group is changed or removed.
A repository referenced by a cleanup policy or webhook cannot be deleted until
that policy or webhook is changed or removed. The API returns HTTP 409 with
`repository_in_use_by_cleanup_policy` or `repository_in_use_by_webhook`.

A repository's format, type, and proxy upstream endpoint are fixed at creation. Proxy
credentials in URL user information may rotate without changing the endpoint. To change
the endpoint or type, create a new repository and move clients to it. A deleted name may
be reused, but the new repository starts with separate assets and cache state; an
in-flight operation on the old repository cannot access the replacement.

An OCI repository with an empty or partially filled upload session cannot be deleted.
Cancel active sessions through the OCI upload URL, or allow expiry and reaping to remove
them before retrying deletion. This keeps staged backend bytes discoverable for cleanup.

## Replacing existing assets

Hosted repositories expose `allowOverwrite` in the API, provisioning, and repository
editor. For example, create a raw repository that rejects changed content at an
existing path:

```sh
./bin/suxenctl repo create --format raw --allow-overwrite=false releases
```

When omitted on creation, replacement is allowed for Raw, OCI, Maven, and Go and
denied for npm, Cargo, and PyPI. Existing repositories keep their prior behavior.
Omitting the setting on an API update or on an existing provisioning resource
preserves its stored value. Proxy and group repositories reject the setting.

With `allowOverwrite: false`, changed content at an existing artifact identity returns
HTTP 409. An identical-content retry succeeds. For OCI, the setting protects tags;
digest-addressed blobs and manifests always retain their content identity. With
`allowOverwrite: true`, native npm, Cargo, and PyPI publication may replace existing
package artifacts; package artifacts and their companion metadata are committed
atomically. Generic writes still cannot bypass native publication validation.

Mutable format indexes, including Maven metadata and its checksum/signature companions,
remain updateable so a repository can accept new versions. Proxy cache refreshes are
unaffected. The setting does not revoke delete permission: a principal permitted to
delete can remove an artifact before publishing its replacement.

## Raw components

A Raw path is otherwise just a file. To let classification and cleanup treat a
payload and its side files as one version, declare ordered component patterns in
`formatConfig.components`:

```yaml
formatConfig:
  components:
    - pattern: '^(?P<name>(models|tracks)/.+)/(?P<version>[0-9][^/]*)/[^/]+$'
      anchor: '\.(glb|zip)$'
    - pattern: '^(?P<name>client/alpha/[^/]+)/trackmaniac-(?P<version>[^/]+)-[^/]+\.zip$'
```

Each `pattern` is an RE2 expression with the named groups `name` and `version`; the
optional `anchor` is an RE2 expression matched against the asset path. The first
pattern that matches a path with a nonempty name and version sets the asset's
`raw.component` and `raw.version` attributes. When the version capture is the whole
parent directory name, as in `models/blocksets/core/0.2.0/core.glb`, every file in
that directory belongs to one version for cleanup. With an `anchor`, only a matching
file can establish that version, so a directory holding only `SHA256SUMS` is not a
version. When the version is part of a file name, each file is its own version.
Paths that match no pattern keep the default behavior: they have no component
attributes, group by parent directory, and are cleaned up file by file.

Changing the patterns reclassifies the repository's existing assets in the same
transaction. Raw groups reject `formatConfig`. See
[classification and cleanup](classification-cleanup.md#raw-component-retention) for
retention examples.

## OCI registry roots

Every OCI repository is reachable at `/repository/<name>/v2/` on the primary listen
address. Vanilla Docker and similar clients cannot use a path prefix, so an OCI repository
may also bind extra registry roots:

```sh
./bin/suxenctl repo create \
  --format oci \
  --hosts registry.example.com \
  --ports 5000 \
  docker
```

`hosts` match the request Host header without a port. `ports` start extra HTTP listeners
on the same bind address as `SUXEN_LISTEN` (skipping the primary port itself). Extra
listeners serve only `/v2`, `/healthz`, and `/readyz` — not the control plane or UI. When
both match different repositories, the hostname wins. Bare `/v2/` on the primary listener
still maps to the repository named `oci` when no host or port binding claims the request.

Extra OCI listener ports are bound at startup. A repository change to `ports` requires a
restart on every serving replica before the new listener is available. Prefer `hosts` plus
Ingress for multi-replica deployments; extra ports are a single-node convenience.

Helm deployments should declare those bindings on the repository in
`provisioning.document`. The chart derives extra Service ports and extra Ingresses from
that document; `ingress.ociHosts` holds only TLS and annotations. See
[Install with Helm](../operations/install-helm.md).

## Storage, policies, and the control-plane API

Blob-store selection and its safety guards are covered in the
[blob stores guide](blobstores.md). Retention, quarantine, and signing are covered in the
[classification and cleanup](classification-cleanup.md),
[webhooks and gates](webhooks-gates.md), and [provenance](provenance.md) guides.

The complete JSON administration API — repositories, assets, browse, search, pagination,
and every control-plane resource — is documented in the [API guide](../reference/api.md)
and the served OpenAPI document at `/api/openapi.json`. The `suxenctl` synopsis is in the
[CLI reference](../reference/cli.md).
