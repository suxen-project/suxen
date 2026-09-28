# Cargo crates

Three repositories that work together: `cargo-proxy` caches the public
crates.io sparse index (`index.crates.io`), `cargo-hosted` accepts internal
`cargo publish`, and `cargo` (a group over both) serves internal and cached
public crates.

## Server side

```sh
examples/cargo/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop.

## Client side

Point cargo at the sparse index (the trailing slash matters):

```toml
# .cargo/config.toml
[registries.suxen]
index = "sparse+https://suxen.example.com/repository/cargo-hosted/"
credential-provider = "cargo:token"
```

### Publish

Publishing needs write on `cargo-hosted`. Attach the `cargo-publisher` role from
`repo.yaml` to a scoped user (see the other language examples for the
`secretRef` user pattern) and give cargo its token:

```sh
cargo login --registry suxen <token>          # or CARGO_REGISTRIES_SUXEN_TOKEN=<token>
cargo publish --registry suxen
```

cargo sends the token as a scheme-less `Authorization` header, which suxen
accepts as an API token — no special credential format is needed. suxen
recomputes the crate checksum on upload; the index is generated from what you
publish.

Publication accepts up to 1 MiB of incoming JSON metadata and checks that the
generated index entry fits within 8 MiB, including its newline, before storing
anything. The complete publish request is limited to 64 MiB or the configured
upload limit, whichever is smaller. See the [consumer guide](../../docs/guides/consumers.md#cargo-crates)
for registry behavior and limits.

Versions differing only in build metadata (such as `1.0.0+one` and `1.0.0+two`)
cannot both be published to one hosted registry. Proxy and group download URLs
use the exact version spelling advertised by the index.

### Resolve

The example launcher grants anonymous reads for manual exploration. A private registry
uses the same token and `credential-provider` for reads. Depend on a published crate
through the same registry:

```toml
# Cargo.toml
[dependencies]
hello = { version = "0.1", registry = "suxen" }
```

```sh
cargo fetch
```

suxen advertises Cargo registries as authentication-required so Cargo sends its token on
crate downloads. Configure a scoped token for native Cargo clients even when the operator
also grants anonymous repository reads.

### Cache crates.io (source replacement)

To pull normal `crates.io` dependencies through suxen (and cache them), replace
the crates.io source with the proxy:

```toml
# .cargo/config.toml
[source.crates-io]
replace-with = "suxen-proxy"
[registries.suxen-proxy]
index = "sparse+https://suxen.example.com/repository/cargo-proxy/"
```

`cargo fetch` then resolves crates.io dependencies through `cargo-proxy`.

## Test

[`test.bats`](test.bats) covers both halves in a pinned `rust` image:

1. publish a crate with `cargo publish --no-verify` and resolve it privately from a
   consumer crate (`cargo fetch`) — hermetic, no upstream;
2. publish distinct crates to the hosted member and a local source behind a
   proxy, then fetch both through one group registry — hermetic;
3. resolve a public crate (`cfg-if`) through the **proxy** via crates.io source
   replacement — needs network egress and **skips** when crates.io is
   unreachable.

Run it with `bats examples/cargo/test.bats` (needs `bats` and Docker).
