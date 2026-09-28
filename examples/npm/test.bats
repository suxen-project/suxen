#!/usr/bin/env bats
# Boots the example and exercises both halves of the npm group:
#  1. publish to the hosted member and resolve it back (hermetic, no upstream)
#  2. install a public package through the proxy member (needs network; skips)

load "../lib/harness.bash"

setup_file() { export SUXEN_EXAMPLE_PUBLIC_READS=false; suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

NODE_IMAGE="node:22-alpine"

@test "npm publish then resolve round-trips through the hosted registry" {
	require_docker
	run client "$NODE_IMAGE" sh -c '
		set -e
		reg="$SUXEN_URL/repository/npm-hosted/"
		host="${SUXEN_URL#http://}"
		cat > /work/.npmrc <<EOF
registry=${reg}
//${host}/repository/npm-hosted/:_authToken=${SUXEN_TOKEN}
EOF
		mkdir -p /work/pkg && cd /work/pkg
		cat > package.json <<EOF
{"name":"suxen-demo","version":"1.0.0","description":"demo","license":"MIT"}
EOF
		npm publish
		test "$(npm view suxen-demo version --registry "$reg")" = "1.0.0"
		cd /work && npm pack suxen-demo@1.0.0 --registry "$reg"
	'
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/suxen-demo-1.0.0.tgz" ]
}

@test "a hosted package resolves through a proxy whose upstream is this suxen" {
	require_docker
	# A hermetic proxy demonstration: publish to the hosted registry, then front
	# it with a proxy repository whose upstream is this same suxen. No external
	# registry is involved. This exercises the proxy read pipeline and the
	# rewriting of dist.tarball back to the proxy under the client's hostname.
	# The upstream carries the example credentials because hosted reads are
	# authenticated here (SUXEN_EXAMPLE_PUBLIC_READS=false).
	cat > "$SUXEN_EXAMPLE_DATA/npm-selfproxy.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: npm-selfproxy
    spec:
      format: npm
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/npm-hosted
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/npm-selfproxy.yaml"

	run client "$NODE_IMAGE" sh -c '
		set -e
		host="${SUXEN_URL#http://}"
		cat > /work/.npmrc <<EOF
//${host}/repository/npm-hosted/:_authToken=${SUXEN_TOKEN}
//${host}/repository/npm-selfproxy/:_authToken=${SUXEN_TOKEN}
EOF
		mkdir -p /work/proxied && cd /work/proxied
		cat > package.json <<EOF
{"name":"suxen-proxied","version":"2.0.0","description":"demo","license":"MIT"}
EOF
		npm publish --registry "$SUXEN_URL/repository/npm-hosted/"
		# Resolve the just-published package through the self-upstream proxy.
		cd /work && npm pack suxen-proxied@2.0.0 --registry "$SUXEN_URL/repository/npm-selfproxy/"
	'
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/suxen-proxied-2.0.0.tgz" ]
}

@test "the npm group serves hosted and hermetic proxy packages" {
	require_docker
	cat > "$SUXEN_EXAMPLE_DATA/npm-group.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: npm-group-source
    spec:
      format: npm
      type: hosted
  - kind: repository
    name: npm-group-proxy
    spec:
      format: npm
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/npm-group-source
  - kind: repository
    name: npm-hermetic-group
    spec:
      format: npm
      type: group
      members: [npm-hosted, npm-group-proxy]
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/npm-group.yaml"

	run client "$NODE_IMAGE" sh -c '
		set -e
		host="${SUXEN_URL#http://}"
		cat > /work/.npmrc <<EOF
//${host}/repository/npm-hosted/:_authToken=${SUXEN_TOKEN}
//${host}/repository/npm-group-source/:_authToken=${SUXEN_TOKEN}
//${host}/repository/npm-hermetic-group/:_authToken=${SUXEN_TOKEN}
EOF
		mkdir -p /work/group-hosted /work/group-source
		printf "%s\n" "{\"name\":\"suxen-group-hosted\",\"version\":\"1.0.0\"}" > /work/group-hosted/package.json
		printf "%s\n" "{\"name\":\"suxen-group-proxy\",\"version\":\"1.0.0\"}" > /work/group-source/package.json
		cd /work/group-hosted
		npm publish --registry "$SUXEN_URL/repository/npm-hosted/"
		cd /work/group-source
		npm publish --registry "$SUXEN_URL/repository/npm-group-source/"
		cd /work
		group="$SUXEN_URL/repository/npm-hermetic-group/"
		test "$(npm view suxen-group-hosted version --registry "$group")" = "1.0.0"
		test "$(npm view suxen-group-proxy version --registry "$group")" = "1.0.0"
		npm pack suxen-group-hosted@1.0.0 --registry "$group"
		npm pack suxen-group-proxy@1.0.0 --registry "$group"
	'
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/suxen-group-hosted-1.0.0.tgz" ]
	[ -f "$SUXEN_EXAMPLE_DATA/suxen-group-proxy-1.0.0.tgz" ]
}

@test "a public package installs through the proxy" {
	require_docker
	# is-number is a tiny, zero-dependency package; fetched from npmjs.org via
	# the private proxy member with npm's bearer-token configuration.
	run client "$NODE_IMAGE" sh -c '
		set -e
		host="${SUXEN_URL#http://}"
		cat > /work/.npmrc <<EOF
//${host}/repository/npm-proxy/:_authToken=${SUXEN_TOKEN}
EOF
		cd /work && npm pack is-number@7.0.0 --registry "$SUXEN_URL/repository/npm-proxy/"
	'
	if [ "$status" -ne 0 ]; then
		skip "npm registry not reachable (offline?): ${output}"
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/is-number-7.0.0.tgz" ]
}
