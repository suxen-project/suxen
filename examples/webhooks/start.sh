#!/usr/bin/env bash
# Start the webhook receiver support stack, then a local suxen that delivers
# events to it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose="docker compose -f $here/compose.yaml"

$compose up -d --build --wait

launcher_pid=""
cleanup() {
	[ -n "$launcher_pid" ] && kill "$launcher_pid" 2>/dev/null || true
	$compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# The receiver listens on the loopback address, which suxen's outbound client
# denies by default (SSRF protection); allow it explicitly for this example.
export SUXEN_OUTBOUND_ALLOWED_HOSTS="127.0.0.1"

"$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@" &
launcher_pid=$!
wait "$launcher_pid"
