#!/usr/bin/env bash
# Start a local suxen; scrape its Prometheus metrics at GET /metrics.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@"
