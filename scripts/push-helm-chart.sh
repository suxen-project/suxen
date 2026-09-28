#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s CHART_ARCHIVE OCI_REGISTRY\n' "${0##*/}" >&2
  exit 2
fi

helm_bin="${HELM_BIN:-helm}"
chart_archive="$1"
oci_registry="$2"

# Helm 3.16 writes registry progress, including the pushed digest, to stderr.
# Capture both streams so the release contract survives that behavior and a
# future stream change, then replay the diagnostics to the workflow log.
set +e
push_output="$("${helm_bin}" push "${chart_archive}" "${oci_registry}" 2>&1)"
push_status=$?
set -e
printf '%s\n' "${push_output}" >&2
if [[ "${push_status}" -ne 0 ]]; then
  exit "${push_status}"
fi

mapfile -t digests < <(
  awk '$1 == "Digest:" && $2 ~ /^sha256:[[:xdigit:]]{64}$/ { print tolower($2) }' \
    <<<"${push_output}"
)
if [[ "${#digests[@]}" -ne 1 ]]; then
  printf 'expected exactly one sha256 chart digest, found %d\n' "${#digests[@]}" >&2
  exit 1
fi
printf '%s\n' "${digests[0]}"
