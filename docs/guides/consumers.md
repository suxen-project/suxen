# Connecting as a consumer

This guide is for developers and CI systems that consume artifacts from a suxen an
operator already runs. You need the server URL and, unless the repository allows anonymous
reads, a credential. If you are standing up your own instance, see
[getting started](../getting-started.md).

## Credentials

suxen accepts a local username and password (Basic authentication), an API token sent as
Bearer authentication, or an OIDC Bearer ID token. An API token is not a Basic-auth
password: clients that only implement Basic authentication need a dedicated local account
and its password. For non-interactive clients that support Bearer tokens, ask your operator
for a scoped API token. Fresh installations require credentials for reads, uploads, deletes,
and control-plane calls; an operator may grant anonymous reads to selected repositories.

```sh
export SUXEN_URL=https://suxen.example
export SUXEN_TOKEN=your-scoped-token
```

## OCI images

The OCI repository is served at the standard Distribution root, so standard tooling works
directly. Clients can list images with `GET /v2/_catalog` and tags with
`GET /v2/<name>/tags/list`. The root challenges Docker with Bearer and Basic during the
handshake. When the operator has granted the `anonymous` role repository read and configured
`SUXEN_OIDC_STATE_SECRET`, `docker pull` works without login; log in before pushing.
ORAS, Crane, and curl can also pull anonymously after that explicit grant:

```sh
docker pull suxen.example/example/alpine:latest
docker login -u <username> suxen.example
docker push suxen.example/example/alpine:latest
```

`docker login` authenticates with a local account password (or an OIDC password grant when
the operator enables one), not an API token. The default `oci` repository is the `/v2/`
root on the primary listen port. Other OCI
repositories can also be registry roots when the operator binds them to a hostname or
extra listen port — for example `docker login registry.example.com` or
`localhost:5000`. Ask your operator which host or port maps to which repository.

Podman, Buildah, ORAS, and Helm's OCI client work the same way. If your client cannot use
the host root, a repository is also reachable at `/repository/<name>/v2/`.

When pushing against a non-loopback host, your client must trust the server's TLS
certificate; run production suxen behind TLS. For a plain-HTTP development instance,
configure the client's insecure-registry setting.

## Raw files

```sh
# With the CLI
./bin/suxenctl raw get raw source/project-v1.tar.gz ./out.tar.gz

# With curl
curl -H "Authorization: Bearer $SUXEN_TOKEN" --output out.tar.gz \
  "$SUXEN_URL/repository/raw/source/project-v1.tar.gz"

curl -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file build.tar.gz \
  "$SUXEN_URL/repository/raw/source/build.tar.gz"
```

## Go modules

A `go` repository speaks the GOPROXY protocol. Go 1.24 and newer can attach the
recommended API token with a `GOAUTH` command. For example, save this as
`~/.config/suxen/goauth` and make it executable:

```sh
#!/bin/sh
printf '%s\n\nAuthorization: Bearer %s\n\n' \
  'https://suxen.example.com/repository/goproxy' "$SUXEN_TOKEN"
```

```sh
export GOAUTH="$HOME/.config/suxen/goauth"
export GOPROXY=https://suxen.example.com/repository/goproxy
go mod download
```

GOAUTH is used only for HTTPS module requests. As a Basic-auth alternative, put a
dedicated local account in `~/.netrc` (`machine suxen.example.com login <username>
password <account-password>`) and restrict the file to the owner. Do not put an API token
in the `.netrc` password field.

A proxy repository caches an upstream module proxy such as
`https://proxy.golang.org`: module files (`.info`, `.mod`, `.zip`) are cached
for good, version lists and `@latest` are re-read on the proxy TTL. Requests
under `sumdb/` are forwarded to the upstream, so checksum verification against
`sum.golang.org` keeps working when the upstream proxies the checksum database
(`proxy.golang.org` does). Against an upstream that does not, set
`GOSUMDB=off` or `GONOSUMDB` for the modules concerned, or let the toolchain
reach `sum.golang.org` directly.

A hosted repository takes `.info`, `.mod`, and `.zip` uploads for canonical
versions at `<module>/@v/<version>.<ext>` and synthesizes `@v/list` and
`@latest` once all three files for a version are present. Individual uploaded
files remain directly readable during publication. A group merges the version
lists of its members and chooses `@latest` by preferring releases, then
prereleases, then pseudo-versions.
Among pseudo-versions, the most recent commit time wins even if its base version
is lower.

Versions must follow Go's module-path rules: v2 and later normally require the
matching `/v2` (or later major) suffix. Legacy releases with `+incompatible` are
accepted and remain visible through group version lists.

## npm packages

An `npm` proxy repository caches an upstream registry such as
`https://registry.npmjs.org`. Point npm at it and supply a token:

```sh
npm config set registry https://suxen.example.com/repository/npm/
npm config set //suxen.example.com/repository/npm/:_authToken <token>
```

Packuments are re-read on the proxy TTL and served with every `dist.tarball`
pointing back at the same suxen hostname the client used, so lockfiles resolve
through suxen; tarballs are cached for good, fetched from whichever host the
upstream packument names. When a packument advertises `dist.integrity`, the
proxy checks the strongest supported hash before caching a tarball; when only
`dist.shasum` is present it checks that SHA-1 hash. A mismatch is rejected,
and a changed upstream URL or advertised hash set gets a separate cache entry.
If the packument is deleted, a previously cached tarball remains available
when exactly one cache entry matches its public path. If several historical
entries match, suxen fetches from the configured upstream rather than choosing
one without the packument's URL and hash information.
The same cached tarball remains available when a refreshed packument removes
its version. If its historical cache identity is missing or ambiguous, the
tarball request returns 404 rather than fetching an unrelated upstream path.
The proxy requests the full upstream packument for every client and caches that
representation, so an install request cannot remove descriptions or readmes
from later metadata reads. Cached abbreviated responses from older suxen
versions are replaced on the next packument read, regardless of the proxy TTL.
Proxy and group reads also handle historical npm names such as `JSONStream`
that contain uppercase letters or other URL-safe legacy characters. Their
tarball links still point through suxen. Hosted publication keeps the current
lowercase package-name rules; legacy names remain readable, not newly
publishable.
A group merges the packuments of its members (versions unioned, `latest` the
highest release). For a tarball advertised by a merged packument, the group
downloads it from the first member whose current packument advertises that
version. An expired proxy packument is refreshed before this choice, so a
cached tarball from a member that removed the version cannot shadow a later
member's release. If a member's packument cannot be refreshed, the merged
packument and group tarball request return an upstream error rather than
choosing bytes that may disagree with the advertised checksum. Direct tarball
requests still use first-match resolution when no member has a packument.
The registry search and audit endpoints are not proxied.

Proxy and group packuments are buffered to rewrite tarball URLs and select the
advertising group member. A cached packument or hosted group source may be at
most 128 MiB. A group's combined sources and a rewritten proxy or group
response may each be at most 256 MiB; direct hosted packuments are capped at
128 MiB. Requests above these bounds fail instead of returning upstream
tarball URLs or a partial group index. Other npm asset paths retain the 8 MiB
buffered-content limit. Hosted publication still limits each version's metadata
record to 8 MiB; its packument is assembled from those records.

A **hosted** `npm` repository accepts `npm publish`. Point npm at it with a
token that has write access and publish as usual:

```sh
npm config set registry https://suxen.example.com/repository/npm-hosted/
npm config set //suxen.example.com/repository/npm-hosted/:_authToken <token>
npm publish
```

Each published version is stored as its tarball plus a small metadata record;
the packument is served with every `dist.tarball` pointing at the suxen
hostname the client used, and `dist.integrity`/`shasum` are passed through
unchanged after verification against the uploaded tarball. A supplied SHA-1,
SHA-256, SHA-384, or SHA-512 integrity hash, and a supplied SHA-1 shasum, must
match the attachment. `dist-tags.latest` is the highest published release — custom
publish tags (`npm publish --tag <tag>`) are not preserved.

Hosted npm repositories require the native publish protocol. Generic asset
PUTs are rejected, including tarball and per-version metadata writes, so they
cannot bypass publication checks. Replacing a published package requires the
repository setting `allowOverwrite: true`; the default is false.

Publication versions must be complete SemVer, at most 256 characters long,
with major, minor, and patch numbers no greater than `9007199254740991`, as
required by npm. Generated `latest` tags ignore versions outside these bounds.
Each version's metadata must match the package name and version being published. Malformed publications are
rejected before any package assets are stored.

## Python packages

A `pypi` proxy repository caches a simple index such as `https://pypi.org`
together with the distribution files it links, even though PyPI serves those
from `files.pythonhosted.org`. Point pip at the repository's `simple/` tree:

```sh
pip config set global.index-url https://suxen.example.com/repository/pypi/simple/
```

For a private repository, give pip a dedicated local account through its keyring or
`~/.netrc`:

```text
machine suxen.example.com login <username> password <account-password>
```

pip uses Basic authentication here, so the password must be the account password, not an
API token. Project pages (JSON or HTML) are re-read
on the proxy TTL and served with every file link pointing back at the same
suxen hostname the client used; files and their PEP 658
`.metadata` companions are cached immutably. Works for any
index layout (PyPI, devpi, private mirrors), since a file's cache path names
its upstream location and exact query. A distribution already cached remains
available if its project page is later deleted or no longer links it. To revoke
that distribution, delete its cached asset. Before caching a distribution or
`.metadata` companion, Suxen verifies the strongest supported advertised hash
(SHA-512, SHA-384, SHA-256, or SHA-1). A mismatch is rejected without publishing
cache state, so a later corrected download can succeed. A changed advertised hash
gets a separate cache identity. Without an advertising index, only an unambiguous
retained identity can be served. New file URLs still need to be
advertised by a cached project page before suxen fetches them. A group merges
the project pages of its members (one entry per file name). Downloads and metadata
companions use the member that supplies that file in the merged project page.
An unavailable member index causes the group request to fail, even if a different
member has cached bytes for the same URL; expired indexes must revalidate before
the group can select a consistent source.
Unlike a direct proxy read, a group file request without a known project-index
candidate returns 404: retained bytes alone cannot establish which member owns
the filename. Read the group project page first to populate its proxy indexes.

Cached proxy indexes and merged group indexes are rendered as HTML or JSON for
the current request's `Accept` header, independently of which representation
originally populated the cache. Negotiated index responses include `Vary: Accept`.
JSON indexes include `meta.api-version`, project/file arrays, and a `hashes` object
for every file, including an empty object when no digest was advertised. HTML
can carry one selected digest fragment; JSON retains all advertised hashes.
An explicit `core-metadata: false` remains unavailable in either representation;
HTML omits the metadata attribute. The modern key takes precedence over the legacy
`dist-info-metadata` alias.
JSON `versions` entries without files survive proxy rewriting; groups union those
entries across members. A quarantined member makes the merged project
quarantined regardless of member order: the group exposes no distribution
links or cached downloads for that project. Other statuses use the first
member's status. A direct proxy also hides files listed by a quarantined
index. Retained proxy files whose old index was removed or refreshed to an
empty quarantined page have no reliable project attribution; delete their
cached assets to revoke them. JSON pages advertise Simple API 1.1 or newer
only when every file has a known size; HTML-derived or mixed pages without sizes advertise 1.0 and
retain newer fields for clients that detect them.
HTML and JSON preserve the standard GPG signature flag, provenance URL, and
project status when those fields can be represented. An advertised `gpg-sig: true`
permits the adjacent `.asc` companion through a proxy or group with the file's
exact query. Suxen caches its bytes but does not verify the signature. Provenance
URLs remain external absolute links; Suxen neither rewrites nor fetches them.

PyPI root source indexes may be at most 128 MiB; project source indexes retain
the ordinary 8 MiB input limit. Root and project responses in either HTML or
JSON may each be at most 128 MiB after link rewriting. A group's combined
index sources may be at most 256 MiB. Requests above these bounds fail rather
than allocating or serving a partial index. Large-index rendering is limited
to one proxy rewrite or final group merge per replica at a time; group source
collection and hosted synthesis can overlap.

A **hosted** `pypi` repository accepts `twine upload`. twine authenticates with
HTTP Basic auth, which suxen resolves against user accounts (not API tokens), so
supply the username and password of an account with write access. Configure the
repository URL (with its trailing slash), then upload:

```sh
twine upload --repository-url https://suxen.example.com/repository/pypi-hosted/ \
  -u <username> -p <password> dist/*
```

Each uploaded distribution is stored under `packages/<project>/`; the root
index and project pages are synthesized per request (HTML or JSON per the
client's `Accept`) with every file link pointing at the same suxen hostname the
client used and a `#sha256=` fragment computed from the stored bytes, so
`pip install --index-url https://suxen.example.com/repository/pypi-hosted/simple/`
resolves and verifies through suxen.

Hosted PyPI repositories require the native upload protocol. Generic asset
PUTs are rejected, including distribution and index writes, so they cannot
bypass publication checks. Replacing a published distribution requires
`allowOverwrite: true`; the default is false.
The normalized project name in an upload must agree with the distribution
filename; mismatches are rejected before the file is stored. When the client
supplies `sha256_digest`, it must match the uploaded bytes.

## Cargo crates

A `cargo` proxy repository caches a sparse registry such as
`https://index.crates.io` together with the crate files it points at, even
when the registry serves them from another host (crates.io uses
`static.crates.io`). Declare it as a registry in `.cargo/config.toml`:

```toml
[registries.suxen]
index = "sparse+https://suxen.example.com/repository/crates/"
credential-provider = "cargo:token"
```

and depend on crates with `registry = "suxen"`, or replace crates.io entirely:

```toml
[source.crates-io]
replace-with = "suxen"

[source.suxen]
registry = "sparse+https://suxen.example.com/repository/crates/"
```

The registry `config.json` and index files are re-read on the proxy TTL, crate
files are cached for good, and the served `config.json` points downloads back
at the same suxen hostname the client used. A cached crate remains downloadable
if `config.json` or its index entry is later deleted, provided its retained cache
identity is unambiguous. New crate downloads require both the config and an index
entry. The proxy checks the advertised SHA-256 checksum before caching; a mismatch
is rejected and a later request can retry. A changed download URL or checksum uses
a separate cache identity. The `yank` and `search` endpoints
are not proxied. A group merges the index entries of its members, taking the first
entry for each version identity (ignoring build metadata), and serves the crate
from the first member whose current index advertises that version. Member index
refresh errors fail the group index and crate download; a missing crate in the
selected member does not fall through to another member's bytes. Group inventory
uses stored indexes without triggering upstream refreshes. suxen advertises Cargo
registries as authentication-required so Cargo includes its token on crate-file
downloads; configure a scoped read token even when an operator has also granted
anonymous repository reads.

A **hosted** `cargo` repository accepts `cargo publish`. Declare it as a
registry and log in with a token that has write access:

```toml
[registries.suxen]
index = "sparse+https://suxen.example.com/repository/cargo-hosted/"
credential-provider = "cargo:token"
```

```sh
cargo login --registry suxen <token>   # or CARGO_REGISTRIES_SUXEN_TOKEN=<token>
cargo publish --registry suxen
```

Each published crate is stored with a per-version index entry; the sparse
index files and `config.json` are synthesized per request under the hostname
the client used, and the checksum is computed from the stored crate. suxen
accepts cargo's scheme-less `Authorization` token as an API token, so no
special credential format is needed.

The publish request is limited to 64 MiB (or the configured upload limit, if
smaller), with at most 1 MiB of incoming JSON metadata. The generated index
entry, including its trailing newline, may occupy at most 8 MiB. Publication
checks that generated size before storing the crate, metadata, or version
claim; an oversized entry returns `413`. Stored entries use the same read
limit, so metadata expansion cannot silently hide an accepted version or
break an identical retry. A group reads at most 8 MiB from each member's
complete Cargo index; exceeding that aggregate limit fails the request.

Hosted Cargo repositories require the native publish protocol. Generic asset
PUTs are rejected, including crate and index metadata writes, so they cannot
bypass publication checks. Replacing a published crate requires
`allowOverwrite: true`; the default is false.

Published versions must be complete SemVer versions, such as `1.2.3` or
`1.2.3-rc.1+build.7`. Major, minor, and patch numbers must fit Cargo's unsigned
64-bit range (at most `18446744073709551615`); index generation and merging
exclude versions outside this range. Numeric prerelease identifiers are not
subject to that integer limit. Build metadata is retained in the index and download URL,
but does not create a distinct version: publishing `1.2.3+other` after `1.2.3`
returns `409 Conflict` even when replacement is enabled. Replacement permits new
content for the same exact crate-name and version spelling; it does not create
another spelling of an existing identity.

## Git snapshots

A `git` proxy repository serves upstream git repositories as snapshots: each
clone or fetch streams one shallow, self-contained pack for the commit you ask
for, cached by commit. Point the clone at the repository followed by the
upstream repository path:

```sh
git clone --depth 1 https://suxen.example.com/repository/github/acme/widget.git
git clone --depth 1 --branch v1.2.3 https://suxen.example.com/repository/github/acme/widget.git
```

The selected ref can be a branch, a lightweight tag, or an annotated tag.
Annotated tag clones retain the tag object and use its commit as the shallow
history boundary. A root commit has no earlier history to truncate and needs no
shallow boundary. The `git.commit` asset attribute records the known commit
boundary; complete root snapshots omit it rather than report a tag's object ID
as a commit.

For a private repository, configure a Git credential helper for the suxen host with a
dedicated local username and account password. Git sends those credentials with Basic
authentication; an API token is not a valid Basic password.

For a proxy whose upstream is `https://github.com`, that clones
`github.com/acme/widget`. CI checkouts (`actions/checkout`, Docker build git
contexts) work unchanged because they fetch a single ref at depth 1. The
repository serves protocol version 2 only (git 2.18 or newer).

What you get is a snapshot, not a mirror: the clone is shallow with no history,
every fetch downloads the whole snapshot again, and a fetch can name one
commit. A plain `git clone` of a repository with several branches or tags is
refused with a message pointing at `--depth 1`, `--single-branch`, or
`--branch`. Pushes are refused. Branch tips are re-read from the upstream on
the proxy TTL; commits and tags are cached for good.

Because every [AUR](https://aur.archlinux.org) package is a single-branch git
repository at `aur.archlinux.org/<pkg>.git`, a `git` proxy pointed at
`https://aur.archlinux.org` mirrors its build recipes: clone one through suxen
with `git clone --single-branch .../repository/aur/<pkg>.git` and build it with
`makepkg` as usual. This mirrors the `PKGBUILD` recipe, not a built package, and
the AUR's RPC search endpoint is not proxied, so clone by exact name. See
[examples/git](../../examples/git/) for a ready-to-apply document.

## Maven and other plugin formats

When the operator has enabled the `maven` format, configure your build tool's repository
URL to the repository endpoint. For private repositories, add a `<server>` to Maven's
`settings.xml` whose `id` matches the repository id, using a dedicated local username and
account password. Gradle's `credentials` block uses the same values. These clients use
Basic authentication and cannot use an API token as the password. The complete `suxenctl`
command set is in the [CLI reference](../reference/cli.md), and the authorization model —
including how to check your own effective privileges with `suxenctl whoami` — is in the
[authorization guide](authorization.md).

Generated Maven metadata and retention use Maven 3.9.16's version comparison
rules, including numeric qualifiers, Unicode decimal digits, and UTF-16
qualifier ordering. Unicode casing follows the Go/x/text tables used to build
suxen; results for newly added Unicode characters can differ from Maven on
a JVM with older Unicode tables.

Maven artifact IDs may end in `-SNAPSHOT`. That suffix alone does not make an
artifact-level `maven-metadata.xml` a snapshot index. Version policies apply
to artifact coordinates; metadata paths alone cannot unambiguously identify
a release or snapshot and remain accepted under either policy.

If a metadata path has both direct timestamped snapshot files and nested version
directories, generated metadata describes the direct snapshot files. An explicitly
uploaded metadata document takes precedence over either generated interpretation.
