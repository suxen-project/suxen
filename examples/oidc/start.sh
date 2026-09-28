#!/usr/bin/env bash
# Start the Dex IdP support stack, then a local suxen federated to it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose="docker compose -f $here/compose.yaml"

$compose up -d

launcher_pid=""
cleanup() {
	[ -n "$launcher_pid" ] && kill "$launcher_pid" 2>/dev/null || true
	$compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# Wait for Dex discovery to answer (the dex image has no shell for a compose
# healthcheck, so poll from the host).
issuer="http://127.0.0.1:5556/dex"
ready=""
for _ in $(seq 1 60); do
	if curl -fsS -o /dev/null "$issuer/.well-known/openid-configuration" 2>/dev/null; then ready="yes"; break; fi
	sleep 1
done
[ -n "$ready" ] || { echo "dex did not become ready" >&2; exit 1; }

# suxen's outbound client denies loopback by default (SSRF protection); the IdP
# lives there in this example, so allow it explicitly. The client secret is
# resolved from the environment when repo.yaml is applied.
export SUXEN_OUTBOUND_ALLOWED_HOSTS="127.0.0.1"
export SUXEN_OIDC_CLIENT_SECRET="suxen-oidc-client-secret"

"$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@" &
launcher_pid=$!
wait "$launcher_pid"
