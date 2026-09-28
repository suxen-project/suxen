#!/usr/bin/env bash
# Print the contract surface version matrix from the manifest, one "id version"
# per line, for inclusion in release notes. Pass a manifest path to override the
# default.
set -euo pipefail

manifest="${1:-internal/contract/versions.yaml}"

if [[ ! -f "${manifest}" ]]; then
	echo "manifest not found: ${manifest}" >&2
	exit 1
fi

awk '
	/^[[:space:]]*-[[:space:]]*id:/ {
		id = $0
		sub(/.*id:[[:space:]]*/, "", id)
		next
	}
	/^[[:space:]]*version:/ && id != "" {
		version = $0
		sub(/.*version:[[:space:]]*/, "", version)
		printf "%-16s %s\n", id, version
		id = ""
	}
' "${manifest}"
