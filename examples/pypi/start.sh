#!/usr/bin/env bash
# Start a local suxen with this example's resources applied.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Permit a proxy repository whose upstream is this same suxen (a hermetic
# proxy-of-a-suxen-registry demonstration): the hardened outbound client
# otherwise refuses loopback destinations.
export SUXEN_OUTBOUND_ALLOWED_HOSTS="${SUXEN_OUTBOUND_ALLOWED_HOSTS:-127.0.0.1}"
exec "$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@"
