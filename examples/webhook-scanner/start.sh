#!/usr/bin/env bash
# Start the scanner support stack, then a suxen that quarantines downloads until
# the scanner (reacting to asset webhooks) stamps a scan attribute back.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose="docker compose -f $here/compose.yaml"

port="${SUXEN_EXAMPLE_PORT:-8080}"
token="${SUXEN_EXAMPLE_TOKEN:-suxen-example-token-abcdef}"
url="http://127.0.0.1:$port"
suxenctl="$here/../.bin/suxenctl"

# suxen must reach the scanner and the upstream (both on loopback) — its outbound
# client denies loopback by default. The proxy's upstream (with credentials) is
# resolved from this env var at apply time.
export SUXEN_OUTBOUND_ALLOWED_HOSTS="127.0.0.1"
export SUXEN_MIRROR_UPSTREAM="http://upstream:upstream-password@127.0.0.1:9092/files/"

launcher_pid=""
cleanup() {
	[ -n "$launcher_pid" ] && kill "$launcher_pid" 2>/dev/null || true
	$compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

"$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" &
launcher_pid=$!

# Wait until ready, then ensure the resources (incl. the scanner role) exist.
for _ in $(seq 1 150); do
	curl -fsS -o /dev/null "$url/readyz" 2>/dev/null && break
	sleep 0.2
done
SUXEN_URL="$url" SUXEN_TOKEN="$token" "$suxenctl" apply -f "$here/repo.yaml" >/dev/null

# Mint a service-account token scoped to exactly "annotate" — the scanner can
# stamp attributes but cannot push or delete artifacts.
SUXEN_URL="$url" SUXEN_TOKEN="$token" "$suxenctl" user create \
	--password scanner-service-pass --roles scanner scanner >/dev/null 2>&1 || true
# No --scopes: the token inherits the scanner role's privileges (annotate only).
scanner_token="$(SUXEN_URL="$url" SUXEN_TOKEN="$token" "$suxenctl" user token \
	--name scanner-token scanner \
	| grep -o '"token":"[^"]*"' | cut -d'"' -f4)"

# Bring up the scanner (with its scoped token) and the upstream.
SCANNER_TOKEN="$scanner_token" SUXEN_INTERNAL_URL="$url" $compose up -d --build

wait "$launcher_pid"
