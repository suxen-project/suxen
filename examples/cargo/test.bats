#!/usr/bin/env bats
# Boots the example, publishes a crate with `cargo publish`, then resolves it
# from a consumer crate. Fully hermetic (hosted, no upstream): only Docker.

load "../lib/harness.bash"

setup_file() { export SUXEN_EXAMPLE_PUBLIC_READS=false; suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

RUST_IMAGE="rust:1-slim"

@test "cargo publish then resolve round-trips through the hosted registry" {
	require_docker
	run client "$RUST_IMAGE" sh -c '
		set -e
		export CARGO_HOME=/work/cargo-home
		mkdir -p "$CARGO_HOME"
		# Sparse index config and the publish token (cargo sends it as a
		# scheme-less Authorization header, which suxen accepts as an API token).
		cat > "$CARGO_HOME/config.toml" <<EOF
[registries.suxen]
index = "sparse+${SUXEN_URL}/repository/cargo-hosted/"
credential-provider = "cargo:token"
EOF
		export CARGO_REGISTRIES_SUXEN_TOKEN="$SUXEN_TOKEN"

		# A crate needs description + license or cargo publish aborts client-side.
		mkdir -p /work/hello/src
		cat > /work/hello/Cargo.toml <<EOF
[package]
name = "suxen-hello"
version = "0.1.0"
edition = "2021"
description = "demo"
license = "MIT"

[dependencies]
EOF
		echo "pub fn hi() {}" > /work/hello/src/lib.rs
		cd /work/hello
		# --no-verify skips the compile; --allow-dirty since there is no VCS.
		cargo publish --registry suxen --no-verify --allow-dirty

		# Resolve it from a consumer crate against the same private registry.
		mkdir -p /work/consumer/src
		cat > /work/consumer/Cargo.toml <<EOF
[package]
name = "consumer"
version = "0.1.0"
edition = "2021"

[dependencies]
suxen-hello = { version = "0.1", registry = "suxen" }
EOF
		echo "fn main() {}" > /work/consumer/src/main.rs
		cd /work/consumer
		cargo fetch
	'
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
}

@test "a hosted crate resolves through a proxy whose upstream is this suxen" {
	require_docker
	# A hermetic proxy demonstration: publish to the hosted registry, then front
	# it with a proxy whose upstream is this same suxen — no crates.io. This
	# exercises cargo proxy resolution and sparse-index rewriting, which points
	# the index config and crate downloads back at the proxy. The upstream
	# carries the example credentials because hosted reads are authenticated
	# here (SUXEN_EXAMPLE_PUBLIC_READS=false).
	cat > "$SUXEN_EXAMPLE_DATA/cargo-selfproxy.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: cargo-selfproxy
    spec:
      format: cargo
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/cargo-hosted
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/cargo-selfproxy.yaml"

	run client "$RUST_IMAGE" sh -c '
		set -e
		export CARGO_HOME=/work/cargo-home-selfproxy
		mkdir -p "$CARGO_HOME"
		cat > "$CARGO_HOME/config.toml" <<EOF
[registries.suxen]
index = "sparse+${SUXEN_URL}/repository/cargo-hosted/"
credential-provider = "cargo:token"
[registries.suxen-selfproxy]
index = "sparse+${SUXEN_URL}/repository/cargo-selfproxy/"
credential-provider = "cargo:token"
EOF
		export CARGO_REGISTRIES_SUXEN_TOKEN="$SUXEN_TOKEN"
		export CARGO_REGISTRIES_SUXEN_SELFPROXY_TOKEN="$SUXEN_TOKEN"

		mkdir -p /work/proxied-crate/src
		cat > /work/proxied-crate/Cargo.toml <<EOF
[package]
name = "suxen-proxied"
version = "0.2.0"
edition = "2021"
description = "demo"
license = "MIT"

[dependencies]
EOF
		echo "pub fn hi() {}" > /work/proxied-crate/src/lib.rs
		cd /work/proxied-crate
		cargo publish --registry suxen --no-verify --allow-dirty

		# Resolve the just-published crate through the self-upstream proxy.
		mkdir -p /work/proxied-consumer/src
		cat > /work/proxied-consumer/Cargo.toml <<EOF
[package]
name = "proxied-consumer"
version = "0.1.0"
edition = "2021"

[dependencies]
suxen-proxied = { version = "0.2", registry = "suxen-selfproxy" }
EOF
		echo "fn main() {}" > /work/proxied-consumer/src/main.rs
		cd /work/proxied-consumer
		cargo fetch
	'
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
}

@test "the Cargo group fetches hosted and hermetic proxy crates" {
	require_docker
	cat > "$SUXEN_EXAMPLE_DATA/cargo-group.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: cargo-group-source
    spec:
      format: cargo
      type: hosted
  - kind: repository
    name: cargo-group-proxy
    spec:
      format: cargo
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/cargo-group-source
  - kind: repository
    name: cargo-hermetic-group
    spec:
      format: cargo
      type: group
      members: [cargo-hosted, cargo-group-proxy]
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/cargo-group.yaml"

	run client "$RUST_IMAGE" sh -c '
		set -e
		export CARGO_HOME=/work/cargo-home-group
		mkdir -p "$CARGO_HOME"
		cat > "$CARGO_HOME/config.toml" <<EOF
[registries.suxen-hosted]
index = "sparse+${SUXEN_URL}/repository/cargo-hosted/"
credential-provider = "cargo:token"
[registries.suxen-source]
index = "sparse+${SUXEN_URL}/repository/cargo-group-source/"
credential-provider = "cargo:token"
[registries.suxen-group]
index = "sparse+${SUXEN_URL}/repository/cargo-hermetic-group/"
credential-provider = "cargo:token"
EOF
		export CARGO_REGISTRIES_SUXEN_HOSTED_TOKEN="$SUXEN_TOKEN"
		export CARGO_REGISTRIES_SUXEN_SOURCE_TOKEN="$SUXEN_TOKEN"
		export CARGO_REGISTRIES_SUXEN_GROUP_TOKEN="$SUXEN_TOKEN"
		make_crate() {
			crate="$1"
			mkdir -p "/work/$crate/src"
			cat > "/work/$crate/Cargo.toml" <<EOF
[package]
name = "$crate"
version = "0.3.0"
edition = "2021"
description = "group example"
license = "MIT"
EOF
			printf "pub fn hi() {}\n" > "/work/$crate/src/lib.rs"
		}
		make_crate suxen-group-hosted
		make_crate suxen-group-proxy
		cd /work/suxen-group-hosted
		cargo publish --registry suxen-hosted --no-verify --allow-dirty
		cd /work/suxen-group-proxy
		cargo publish --registry suxen-source --no-verify --allow-dirty
		mkdir -p /work/group-consumer/src
		cat > /work/group-consumer/Cargo.toml <<EOF
[package]
name = "group-consumer"
version = "0.1.0"
edition = "2021"

[dependencies]
suxen-group-hosted = { version = "0.3", registry = "suxen-group" }
suxen-group-proxy = { version = "0.3", registry = "suxen-group" }
EOF
		printf "fn main() {}\n" > /work/group-consumer/src/main.rs
		cd /work/group-consumer
		cargo fetch
	'
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
}

@test "a public crate resolves through the proxy (crates.io source replacement)" {
	require_docker
	run client "$RUST_IMAGE" sh -c '
		set -e
		export CARGO_HOME=/work/cargo-home-proxy
		export CARGO_REGISTRIES_SUXEN_PROXY_TOKEN="$SUXEN_TOKEN"
		mkdir -p "$CARGO_HOME"
		# Replace crates.io with the suxen proxy so normal crates.io deps resolve
		# through suxen and get cached.
		cat > "$CARGO_HOME/config.toml" <<EOF
[source.crates-io]
replace-with = "suxen-proxy"
[registries.suxen-proxy]
index = "sparse+${SUXEN_URL}/repository/cargo-proxy/"
credential-provider = "cargo:token"
EOF
		mkdir -p /work/pub/src
		cat > /work/pub/Cargo.toml <<EOF
[package]
name = "consumer-pub"
version = "0.1.0"
edition = "2021"

[dependencies]
cfg-if = "1"
EOF
		echo "fn main() {}" > /work/pub/src/main.rs
		cd /work/pub && cargo fetch
	'
	if [ "$status" -ne 0 ]; then
		skip "crates.io not reachable (offline?): ${output}"
	fi
	[ "$status" -eq 0 ]
}
