# Examples

Each subdirectory is one focused use case: a small declarative document plus a
README that documents both the **server side** (how to run suxen with that
feature) and the **client side** (how to configure the real client), and a
`test.bats` that runs the client commands and asserts they work.

## The launcher

Every example starts from one command:

```sh
examples/suxen-with-mem-blobstore [-f RESOURCES.yaml]
```

It builds and runs a local suxen **natively** — SQLite metadata and an
in-memory blob store (the [`plugin-memblob`](plugin-memblob/) driver), no
database or object store required — bootstraps an admin, and with `-f` applies a
resource document like `kubectl apply`. It then enables the explicit
[`development/`](development/) profile, which grants anonymous reads only for this
loopback, in-memory process. It prints the URL and admin token and runs in the foreground;
Ctrl-C stops it and the ephemeral data is discarded.

Each example wraps it in a `start.sh`, so running one feature is just:

```sh
examples/raw/start.sh
```

## Running the tests

`test.bats` boots the example through its `start.sh`, then runs the client
commands from the README. Client toolchains run in **pinned Docker images** over
the host network, so you need Docker but not every language toolchain installed:

```sh
bats examples/raw/test.bats
```

A test skips cleanly when Docker (or a required upstream) is unavailable.

## Examples

| Example | What it shows | Client |
| --- | --- | --- |
| [`raw/`](raw/) | Hosted raw file store; authenticated write, development-profile anonymous read | curl |
| [`go/`](go/) | Go module proxy + hosted + group under one GOPROXY | go |
| [`npm/`](npm/) | npm proxy + hosted + group; `npm publish` and cache npmjs.org | npm |
| [`pypi/`](pypi/) | PyPI proxy + hosted + group; `twine upload` and cache pypi.org | twine, pip |
| [`cargo/`](cargo/) | Cargo proxy + hosted + group; `cargo publish` and cache crates.io | cargo |
| [`maven/`](maven/) | Maven proxy + hosted + group; resolve and deploy | maven |
| [`git/`](git/) | Git snapshot proxy (GitHub, AUR, any host); shallow clone | git |
| [`oci/`](oci/) | OCI hosted at the /v2 root + Docker Hub proxy on a bound port | oras, docker, helm |

Each format `test.bats` uses the real client toolchain in a pinned Docker image.
The hosted and local self-proxy round-trips publish their own artifact fixtures.
Running them can still require network access to pull the client images and
download build tooling, such as Maven plugins. The public proxy/cache cases reach
the real registry and **skip** cleanly when it is unavailable.

## Policy features

These bind a policy to a repository (usually the bootstrap `raw` repo) and drive
it with `suxenctl` (the admin client) plus `curl` for the download side.

| Example | What it shows |
| --- | --- |
| [`classification/`](classification/) | Label assets by rules; labels feed other policies |
| [`cleanup/`](cleanup/) | Retention: delete matching assets beyond `keepLast` |
| [`download-gate/`](download-gate/) | Quarantine downloads until an attribute passes (403→200) |
| [`trust-policy/`](trust-policy/) | Provenance: reject unsigned uploads; accept signed |
| [`cosign/`](cosign/) | Provenance with Sigstore cosign: verify-on-push accepts only trusted-key signatures |
| [`policy-defaults/`](policy-defaults/) | Instance-wide default policy + `inheritGlobal`: inherit / AND-extend / opt out |

## Access & configuration

Control-plane features driven by `suxenctl` (+ curl); fully hermetic.

| Example | What it shows |
| --- | --- |
| [`rbac/`](rbac/) | Scoped users + tokens; allowed 200 / out-of-scope 403 / whoami / anon read |
| [`provisioning/`](provisioning/) | `apply` dry-run, prune, managed-resource guard + force, secretRef env/file |
| [`blobstores/`](blobstores/) | Named blob stores, per-repo binding, GC of unreferenced blobs, draining |
| [`metrics/`](metrics/) | Prometheus `/metrics` scrape; `admin:stats:read` gate (401/403/200) and series |

## Support-service features

These need one external service, so each ships a dedicated `compose.yaml`.
suxen still runs natively (via the launcher); `start.sh` brings the stack up,
points suxen at it, and tears it down on exit.

| Example | Support service | What it shows |
| --- | --- | --- |
| [`s3/`](s3/) | MinIO | Blobs stored in an S3 object store instead of memory |
| [`webhooks/`](webhooks/) | HMAC receiver | Signed event delivery on asset changes |
| [`webhook-scanner/`](webhook-scanner/) | Scanner + upstream | Quarantine → webhook → scanner annotates via the API → gate releases (hosted + proxy) |
| [`oidc/`](oidc/) | Dex IdP | Federated login: IdP users authenticate and get mapped to roles |

The [cosign example](cosign/) uses the pinned cosign client to sign artifacts and
verify them against a configured public key; it does not need a support service.

## Out-of-tree plugins

[`plugin-memblob/`](plugin-memblob/) is a different kind of example: a custom
blob-store driver and the packaging pattern for a custom suxen distribution. It
is what the launcher above builds to get the in-memory blob store.

The full resource schema is in
[docs/operations/provisioning.md](../docs/operations/provisioning.md); the
client side of every format is in
[docs/guides/consumers.md](../docs/guides/consumers.md).
