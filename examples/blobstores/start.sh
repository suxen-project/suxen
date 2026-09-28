#!/usr/bin/env bash
# Start a local suxen with a second, filesystem-backed blob store.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The `archive` blob store's URI is resolved server-side from this env var; keep
# it under the (ephemeral) example data dir. A short migrate interval makes a
# drain complete promptly.
: "${SUXEN_EXAMPLE_DATA:=$(mktemp -d)}"; export SUXEN_EXAMPLE_DATA
export SUXEN_ARCHIVE_URI="fs://$SUXEN_EXAMPLE_DATA/archive-blobs"
export SUXEN_MIGRATE_INTERVAL="${SUXEN_MIGRATE_INTERVAL:-1s}"
exec "$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@"
