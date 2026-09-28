# harness.bash — shared helpers for the examples' test.bats files.
#
# Source it from a test:  load "../lib/harness.bash"
# Then:  suxen_start "$BATS_TEST_DIRNAME"   # boots ./start.sh, applies repo.yaml
#        client <image> <cmd...>            # runs a client in a pinned image
#        suxen_stop                          # in teardown
#
# The server runs natively (SQLite + mem blob store); only client toolchains are
# containerized, reached over the host network at $SUXEN_URL.

examples_dir() { cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd; }

# find_free_port prints a TCP port with nothing listening on 127.0.0.1.
find_free_port() {
	local candidate
	for _ in $(seq 1 50); do
		candidate=$(( (RANDOM % 20000) + 20000 ))
		if ! (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then
			echo "$candidate"; return 0
		fi
		exec 3>&- 2>/dev/null || true
	done
	echo 18080
}

# suxen_start EXAMPLE_DIR boots the example's start.sh in the background on a
# free port, waits until the server answers, then re-applies repo.yaml so the
# desired state is guaranteed present before assertions (apply is idempotent).
suxen_start() {
	local example_dir="$1"
	export SUXEN_EXAMPLES; SUXEN_EXAMPLES="$(examples_dir)"
	export SUXEN_EXAMPLE_PORT; SUXEN_EXAMPLE_PORT="$(find_free_port)"
	# Long enough (token >=24, password >=12) for shared-storage examples (s3),
	# which suxen requires; harmless for the SQLite+mem examples.
	export SUXEN_EXAMPLE_TOKEN="suxen-example-token-abcdef"
	# Some clients (twine) send Basic auth, which suxen resolves against the
	# users table, not API tokens — expose the admin username/password too.
	export SUXEN_BOOTSTRAP_USER="admin" SUXEN_BOOTSTRAP_PASSWORD="suxen-example-password"
	export SUXEN_EXAMPLE_USER="admin" SUXEN_EXAMPLE_PASSWORD="suxen-example-password"
	export SUXEN_URL="http://127.0.0.1:$SUXEN_EXAMPLE_PORT"
	export SUXEN_TOKEN="$SUXEN_EXAMPLE_TOKEN"
	export SUXEN_EXAMPLE_DATA="${BATS_FILE_TMPDIR:-${BATS_TEST_TMPDIR:-$(mktemp -d)}}/suxen"

	"$example_dir/start.sh" >"$SUXEN_EXAMPLE_DATA.log" 2>&1 &
	SUXEN_LAUNCHER_PID=$!

	local ready=""
	for _ in $(seq 1 150); do
		# /readyz turns 200 only after bootstrap (admin + token) completes, so
		# the re-apply below authenticates; other routes answer earlier.
		if curl -fsS -o /dev/null "$SUXEN_URL/readyz" 2>/dev/null; then ready="yes"; break; fi
		kill -0 "$SUXEN_LAUNCHER_PID" 2>/dev/null || break
		sleep 0.2
	done
	if [ -z "$ready" ]; then
		echo "suxen did not start; log:" >&2; cat "$SUXEN_EXAMPLE_DATA.log" >&2 || true
		return 1
	fi
	SUXEN_URL="$SUXEN_URL" SUXEN_TOKEN="$SUXEN_TOKEN" \
		"$SUXEN_EXAMPLES/.bin/suxenctl" apply -f "$example_dir/repo.yaml" >/dev/null
	# Every apply also reconciles the private built-in anonymous role. Restore the
	# explicit loopback-only development profile after reapplying an example,
	# unless a client-authentication test requested production-like private reads.
	if [ "${SUXEN_EXAMPLE_PUBLIC_READS:-true}" = "true" ]; then
		SUXEN_URL="$SUXEN_URL" SUXEN_TOKEN="$SUXEN_TOKEN" \
			"$SUXEN_EXAMPLES/.bin/suxenctl" apply \
			-f "$SUXEN_EXAMPLES/development/repo.yaml" >/dev/null
	fi
}

# suxen_enable_tls puts a local TLS reverse proxy in front of the example. The
# Go command deliberately sends GOAUTH credentials only over HTTPS, so its real
# private-repository test uses the same transport boundary as a deployment.
suxen_enable_tls() {
	require_openssl
	local upstream="$SUXEN_URL" tls_port
	tls_port="$(find_free_port)"
	openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
		-keyout "$SUXEN_EXAMPLE_DATA/tls.key" \
		-out "$SUXEN_EXAMPLE_DATA/tls.crt" \
		-subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 \
		>/dev/null 2>&1
	go build -o "$SUXEN_EXAMPLE_DATA/tls-proxy" "$SUXEN_EXAMPLES/lib/tls-proxy.go"
	"$SUXEN_EXAMPLE_DATA/tls-proxy" \
		-listen "127.0.0.1:$tls_port" -target "$upstream" \
		-cert "$SUXEN_EXAMPLE_DATA/tls.crt" \
		-key "$SUXEN_EXAMPLE_DATA/tls.key" \
		>"$SUXEN_EXAMPLE_DATA/tls-proxy.log" 2>&1 &
	SUXEN_TLS_PROXY_PID=$!
	local tls_url="https://127.0.0.1:$tls_port" ready=""
	for _ in $(seq 1 100); do
		if curl --cacert "$SUXEN_EXAMPLE_DATA/tls.crt" -fsS -o /dev/null \
			"$tls_url/readyz" 2>/dev/null; then ready="yes"; break; fi
		kill -0 "$SUXEN_TLS_PROXY_PID" 2>/dev/null || break
		sleep 0.1
	done
	if [ -z "$ready" ]; then
		echo "TLS proxy did not start; log:" >&2
		cat "$SUXEN_EXAMPLE_DATA/tls-proxy.log" >&2 || true
		return 1
	fi
	export SUXEN_URL="$tls_url"
	export SUXEN_CLIENT_CA_FILE=/work/tls.crt
}

# suxen_stop terminates the launcher; its trap stops the server and clears data.
suxen_stop() {
	if [ -n "${SUXEN_TLS_PROXY_PID:-}" ]; then
		kill "$SUXEN_TLS_PROXY_PID" 2>/dev/null || true
		wait "$SUXEN_TLS_PROXY_PID" 2>/dev/null || true
	fi
	[ -n "${SUXEN_LAUNCHER_PID:-}" ] || return 0
	kill "$SUXEN_LAUNCHER_PID" 2>/dev/null || true
	wait "$SUXEN_LAUNCHER_PID" 2>/dev/null || true
}

# suxenctl runs the built admin client (host binary) against the running server;
# SUXEN_URL/SUXEN_TOKEN come from the environment suxen_start exported.
suxenctl() { "$SUXEN_EXAMPLES/.bin/suxenctl" "$@"; }

# asset_id REPO PATH prints the numeric id of the asset at PATH. It filters by
# PATH as a prefix, so use a unique path per assertion.
asset_id() {
	suxenctl repo assets "$1" "$2" \
		| grep -o '"id"[[:space:]]*:[[:space:]]*[0-9]\+' \
		| grep -o '[0-9]\+' | head -1
}

# asset_digest REPO PATH prints the generation precondition used by scanners
# when writing or deleting an attribute namespace.
asset_digest() {
	suxenctl repo assets "$1" "$2" \
		| grep -o '"digest"[[:space:]]*:[[:space:]]*"[^"]\+"' \
		| head -1 | cut -d '"' -f 4
}

# require_openssl skips the test when the openssl binary is not on the host.
require_openssl() { command -v openssl >/dev/null 2>&1 || skip "openssl not installed"; }

# require_docker skips the test when Docker is not usable.
require_docker() {
	command -v docker >/dev/null 2>&1 || skip "docker not installed"
	docker info >/dev/null 2>&1 || skip "docker not usable"
}

# client IMAGE CMD... runs a client command in a pinned toolchain image on the
# host network, with the test's working files mounted at /work. It runs as the
# host user (so files it creates stay removable on teardown) with HOME=/work (so
# per-tool caches land in the mounted, writable directory).
client() {
	local image="$1"; shift
	docker run --rm --network host \
		--user "$(id -u):$(id -g)" \
		-v "${SUXEN_EXAMPLE_DATA:-$PWD}:/work" -w /work \
		-e HOME=/work \
		-e "SUXEN_URL=$SUXEN_URL" -e "SUXEN_TOKEN=$SUXEN_TOKEN" \
		-e "SUXEN_USER=${SUXEN_EXAMPLE_USER:-admin}" \
		-e "SUXEN_PASSWORD=${SUXEN_EXAMPLE_PASSWORD:-adminpass}" \
		-e "SSL_CERT_FILE=${SUXEN_CLIENT_CA_FILE:-}" \
		-e "CURL_CA_BUNDLE=${SUXEN_CLIENT_CA_FILE:-}" \
		"$image" "$@"
}
