#!/usr/bin/env bats
# Boots the example and exercises the OCI registry with the three clients the
# README documents: oras (artifacts), docker (images), and helm (charts), plus
# the Docker Hub pull-through proxy. The oras/docker/helm round-trips are hermetic
# (hosted, no upstream); the proxy case reaches Docker Hub and skips when offline.

load "../lib/harness.bash"

setup_file() {
	suxen_start "$BATS_TEST_DIRNAME"
	printf 'oci artifact payload\n' >"$SUXEN_EXAMPLE_DATA/notes.txt"
}
teardown_file() {
	# The docker test pushes to the host daemon; drop the image it leaves behind.
	docker rmi "${SUXEN_URL#http://}/dockerdemo:v1" >/dev/null 2>&1 || true
	suxen_stop
}

ORAS_IMAGE="ghcr.io/oras-project/oras:v1.2.2"
HELM_IMAGE="alpine/helm:3.16.4"

@test "oras pushes an artifact to the registry root and pulls it back" {
	require_docker
	# The /v2 API is served at the registry root, so the reference is just
	# host/name:tag — no /repository/ path. Push needs write (admin here);
	# --plain-http because the example runs over http.
	local ref="${SUXEN_URL#http://}/demo/notes:v1"

	run client "$ORAS_IMAGE" push --plain-http \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" \
		"$ref" notes.txt:text/plain
	[ "$status" -eq 0 ]

	run client "$ORAS_IMAGE" pull --plain-http \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" \
		-o /work/pulled "$ref"
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/pulled/notes.txt" ]
	diff "$SUXEN_EXAMPLE_DATA/notes.txt" "$SUXEN_EXAMPLE_DATA/pulled/notes.txt"
}

@test "docker pushes an image to the registry root and pulls it back" {
	require_docker
	# The docker client drives the host daemon (there is no daemon inside a
	# pinned image), so this uses host `docker`. Auth is isolated to the test's
	# own DOCKER_CONFIG so the user's ~/.docker/config.json is untouched. A
	# loopback registry is treated as insecure by the daemon, so plain HTTP works
	# with no daemon configuration.
	local reg="${SUXEN_URL#http://}" ref
	ref="${reg}/dockerdemo:v1"
	export DOCKER_CONFIG="$SUXEN_EXAMPLE_DATA/docker"

	# Build a tiny image with no base pull (FROM scratch), so the test is hermetic.
	mkdir -p "$SUXEN_EXAMPLE_DATA/img"
	printf 'hello from suxen\n' >"$SUXEN_EXAMPLE_DATA/img/hello.txt"
	printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' >"$SUXEN_EXAMPLE_DATA/img/Dockerfile"
	run docker build -t "$ref" "$SUXEN_EXAMPLE_DATA/img"
	[ "$status" -eq 0 ]

	run bash -c "printf '%s' \"$SUXEN_EXAMPLE_PASSWORD\" | docker login '$reg' --username '$SUXEN_EXAMPLE_USER' --password-stdin"
	[ "$status" -eq 0 ]

	run docker push "$ref"; [ "$status" -eq 0 ]

	# Drop the local copy, then pull it back from suxen.
	docker rmi "$ref" >/dev/null 2>&1 || true
	run docker pull "$ref"; [ "$status" -eq 0 ]
	run docker image inspect "$ref"; [ "$status" -eq 0 ]

	docker rmi "$ref" >/dev/null 2>&1 || true
	docker logout "$reg" >/dev/null 2>&1 || true
}

@test "helm pushes a chart to the registry root and pulls it back" {
	require_docker
	local reg="${SUXEN_URL#http://}"

	# Package a fresh starter chart (helm create needs no network).
	run client "$HELM_IMAGE" create demo; [ "$status" -eq 0 ]
	run client "$HELM_IMAGE" package demo; [ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/demo-0.1.0.tgz" ]

	# Charts are OCI artifacts. Login needs --insecure to accept the plain-HTTP
	# registry; push/pull take --plain-http. Credentials persist in the mounted
	# HOME across these container invocations.
	run client "$HELM_IMAGE" registry login "$reg" \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" --insecure
	[ "$status" -eq 0 ]

	run client "$HELM_IMAGE" push /work/demo-0.1.0.tgz "oci://$reg" --plain-http
	[ "$status" -eq 0 ]

	# helm pull -d requires the destination directory to exist.
	mkdir -p "$SUXEN_EXAMPLE_DATA/pull"
	run client "$HELM_IMAGE" pull "oci://$reg/demo" --version 0.1.0 --plain-http -d /work/pull
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/pull/demo-0.1.0.tgz" ]
}

@test "a public image is pulled through the Docker Hub proxy on its bound port" {
	require_docker
	# The dockerhub proxy is bound to 127.0.0.1:5001, so it is a registry root:
	# 127.0.0.1:5001/library/<image>. suxen handles Docker Hub's Bearer-token
	# challenge and caches the result. Anonymous read. Check the local listener
	# first: a missing startup binding is a broken example, not an offline hub.
	run curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
		http://127.0.0.1:5001/v2/
	[ "$status" -eq 0 ]
	[ "$output" = "401" ]

	run client "$ORAS_IMAGE" manifest fetch --plain-http \
		127.0.0.1:5001/library/hello-world:latest
	if [ "$status" -ne 0 ]; then
		# The public registry's /v2/ root deliberately returns 401 when it is
		# reachable. Only a transport failure warrants an offline skip; upstream
		# HTTP errors and local proxy failures must remain test failures.
		if ! curl -sS -o /dev/null --connect-timeout 5 --max-time 10 \
			https://registry-1.docker.io/v2/; then
			skip "Docker Hub not reachable (offline?): ${output}"
		fi
	fi
	[ "$status" -eq 0 ]
	[[ "$output" == *"schemaVersion"* ]]
}

@test "protected OCI tag rejects changed content and retains its original artifact" {
	require_docker
	local ref="${SUXEN_URL#http://}/protected/notes:v1"
	printf 'replacement artifact\n' >"$SUXEN_EXAMPLE_DATA/replacement.txt"
	run client "$ORAS_IMAGE" push --plain-http \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" \
		"$ref" notes.txt:text/plain
	[ "$status" -eq 0 ]
	run client "$ORAS_IMAGE" push --plain-http \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" \
		"$ref" replacement.txt:text/plain
	[ "$status" -ne 0 ]
	[[ "$output" == *"already exists"* ]]
	run client "$ORAS_IMAGE" pull --plain-http \
		--username "$SUXEN_EXAMPLE_USER" --password "$SUXEN_EXAMPLE_PASSWORD" \
		-o /work/protected-pull "$ref"
	[ "$status" -eq 0 ]
	cmp "$SUXEN_EXAMPLE_DATA/notes.txt" "$SUXEN_EXAMPLE_DATA/protected-pull/notes.txt"
}
