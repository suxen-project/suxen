#!/usr/bin/env bash
# Start a local suxen with an instance-wide default download gate.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@"
