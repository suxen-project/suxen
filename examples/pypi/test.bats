#!/usr/bin/env bats
# Boots the example, publishes a wheel with `twine upload`, then installs it
# with pip from the simple/ index. The publish/install round-trip is hermetic
# (hosted, no upstream); only installing twine into the image needs network, so
# the test skips cleanly when that download is unavailable.

load "../lib/harness.bash"

setup_file() {
	export SUXEN_EXAMPLE_PUBLIC_READS=false
	suxen_start "$BATS_TEST_DIRNAME"
	# A minimal, valid wheel so pip installs without a build step.
	cat >"$SUXEN_EXAMPLE_DATA/mkwheel.py" <<'PY'
import base64, hashlib, io, os, sys, zipfile
name = sys.argv[1] if len(sys.argv) > 1 else "suxen_demo"
ver = sys.argv[2] if len(sys.argv) > 2 else "1.0.0"
dist = f"{name}-{ver}"
files = {
    f"{name}/__init__.py": b"value = 42\n",
    f"{dist}.dist-info/METADATA":
        f"Metadata-Version: 2.1\nName: {name.replace('_', '-')}\nVersion: {ver}\n".encode(),
    f"{dist}.dist-info/WHEEL":
        b"Wheel-Version: 1.0\nGenerator: suxen-example\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
}
lines = []
for fn, data in files.items():
    digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
    lines.append(f"{fn},sha256={digest},{len(data)}")
record_name = f"{dist}.dist-info/RECORD"
lines.append(f"{record_name},,")
record = ("\n".join(lines) + "\n").encode()
os.makedirs("/work/dist", exist_ok=True)
path = f"/work/dist/{dist}-py3-none-any.whl"
with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
    # Both scenarios publish this version. Fixed member timestamps make the
    # repeated publication byte-identical, as immutable distributions require.
    for fn, data in {**files, record_name: record}.items():
        info = zipfile.ZipInfo(fn, date_time=(2020, 1, 1, 0, 0, 0))
        z.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED)
print(path)
PY
}
teardown_file() { suxen_stop; }

PY_IMAGE="python:3.12-slim"

pypi_upstream_reachable() {
	client "$PY_IMAGE" python -c '
import urllib.error
import urllib.request
for url in ("https://pypi.org/simple/twine/", "https://files.pythonhosted.org/"):
    try:
        urllib.request.urlopen(url, timeout=10).close()
    except urllib.error.HTTPError:
        pass
' >/dev/null 2>&1
}

@test "twine upload then pip install round-trips through the hosted registry" {
	require_docker
	run client "$PY_IMAGE" sh -c '
		set -e
		# Only the tool install touches the network; skip (3) if it fails.
		python -m pip install --quiet --disable-pip-version-check twine || exit 3
		python /work/mkwheel.py
		host="${SUXEN_URL#http://}"
		private_url="http://${SUXEN_USER}:${SUXEN_PASSWORD}@${host}"
		# twine sends HTTP Basic auth; suxen resolves it against the users table,
		# so authenticate with the admin account, not an API token.
		python -m twine upload --non-interactive \
			--repository-url "$SUXEN_URL/repository/pypi-hosted/" \
			-u "$SUXEN_USER" -p "$SUXEN_PASSWORD" /work/dist/*.whl
		python -m pip install --quiet --disable-pip-version-check \
			--index-url "$private_url/repository/pypi-hosted/simple/" \
			--trusted-host "$host" --target /work/site "suxen-demo==1.0.0"
		test -f /work/site/suxen_demo/__init__.py
	'
	if [ "$status" -eq 3 ]; then
		install_failure="$output"
		if ! pypi_upstream_reachable; then
			skip "PyPI or its file host not reachable"
		fi
		echo "$install_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/site/suxen_demo/__init__.py" ]
}

@test "a hosted distribution downloads through a proxy whose upstream is this suxen" {
	require_docker
	# A hermetic proxy demonstration: publish to the hosted registry, then front
	# it with a proxy whose upstream is this same suxen — no external index. This
	# exercises pypi's proxy request resolution and simple-index rewriting, which
	# points every distribution URL back at the proxy under the client hostname.
	# The upstream carries the example credentials because hosted reads are
	# authenticated here (SUXEN_EXAMPLE_PUBLIC_READS=false).
	cat > "$SUXEN_EXAMPLE_DATA/pypi-selfproxy.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: pypi-selfproxy
    spec:
      format: pypi
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/pypi-hosted
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/pypi-selfproxy.yaml"

	run client "$PY_IMAGE" sh -c '
		set -e
		# Only the tool install touches the network; skip (3) if it fails.
		python -m pip install --quiet --disable-pip-version-check twine || exit 3
		python /work/mkwheel.py
		host="${SUXEN_URL#http://}"
		private_url="http://${SUXEN_USER}:${SUXEN_PASSWORD}@${host}"
		python -m twine upload --non-interactive \
			--repository-url "$SUXEN_URL/repository/pypi-hosted/" \
			-u "$SUXEN_USER" -p "$SUXEN_PASSWORD" /work/dist/*.whl
		# Resolve the just-published wheel through the self-upstream proxy.
		python -m pip download --no-deps --disable-pip-version-check \
			--index-url "$private_url/repository/pypi-selfproxy/simple/" \
			--trusted-host "$host" --dest /work/proxied "suxen-demo==1.0.0"
	'
	if [ "$status" -eq 3 ]; then
		install_failure="$output"
		if ! pypi_upstream_reachable; then
			skip "PyPI or its file host not reachable"
		fi
		echo "$install_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	ls "$SUXEN_EXAMPLE_DATA"/proxied/suxen_demo-*.whl >/dev/null 2>&1
}

@test "the PyPI group serves hosted and hermetic proxy distributions" {
	require_docker
	cat > "$SUXEN_EXAMPLE_DATA/pypi-group.yaml" <<EOF
apiVersion: suxen.io/v1
resources:
  - kind: repository
    name: pypi-group-source
    spec:
      format: pypi
      type: hosted
  - kind: repository
    name: pypi-group-proxy
    spec:
      format: pypi
      type: proxy
      upstream: http://${SUXEN_EXAMPLE_USER}:${SUXEN_EXAMPLE_PASSWORD}@127.0.0.1:${SUXEN_EXAMPLE_PORT}/repository/pypi-group-source
  - kind: repository
    name: pypi-hermetic-group
    spec:
      format: pypi
      type: group
      members: [pypi-hosted, pypi-group-proxy]
EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/pypi-group.yaml"

	run client "$PY_IMAGE" sh -c '
		set -e
		python -m pip install --quiet --disable-pip-version-check twine || exit 3
		python /work/mkwheel.py suxen_group_hosted 1.0.0
		python /work/mkwheel.py suxen_group_proxy 1.0.0
		python -m twine upload --non-interactive \
			--repository-url "$SUXEN_URL/repository/pypi-hosted/" \
			-u "$SUXEN_USER" -p "$SUXEN_PASSWORD" /work/dist/suxen_group_hosted-*.whl
		python -m twine upload --non-interactive \
			--repository-url "$SUXEN_URL/repository/pypi-group-source/" \
			-u "$SUXEN_USER" -p "$SUXEN_PASSWORD" /work/dist/suxen_group_proxy-*.whl
		host="${SUXEN_URL#http://}"
		private_url="http://${SUXEN_USER}:${SUXEN_PASSWORD}@${host}"
		python -m pip download --no-deps --disable-pip-version-check \
			--index-url "$private_url/repository/pypi-hermetic-group/simple/" \
			--trusted-host "$host" --dest /work/group-downloads \
			"suxen-group-hosted==1.0.0" "suxen-group-proxy==1.0.0"
	'
	if [ "$status" -eq 3 ]; then
		install_failure="$output"
		if ! pypi_upstream_reachable; then
			skip "PyPI or its file host not reachable"
		fi
		echo "$install_failure" >&2
		false
	fi
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
	[ -f "$SUXEN_EXAMPLE_DATA/group-downloads/suxen_group_hosted-1.0.0-py3-none-any.whl" ]
	[ -f "$SUXEN_EXAMPLE_DATA/group-downloads/suxen_group_proxy-1.0.0-py3-none-any.whl" ]
}

@test "a public distribution downloads through the proxy" {
	require_docker
	# six is a tiny, zero-dependency package with a wheel; fetched from pypi.org
	# via the proxy member's simple/ index. Anonymous read.
	run client "$PY_IMAGE" sh -c '
		set -e
		host="${SUXEN_URL#http://}"
		private_url="http://${SUXEN_USER}:${SUXEN_PASSWORD}@${host}"
		python -m pip download --no-deps --disable-pip-version-check \
			--index-url "$private_url/repository/pypi-proxy/simple/" \
			--trusted-host "$host" --dest /work/dl six
	'
	if [ "$status" -ne 0 ]; then
		download_failure="$output"
		if ! pypi_upstream_reachable; then
			skip "PyPI or its file host not reachable"
		fi
		echo "$download_failure" >&2
		false
	fi
	[ "$status" -eq 0 ]
	ls "$SUXEN_EXAMPLE_DATA"/dl/six-*.whl >/dev/null 2>&1
}
