#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: check-public-packages.sh VERSION OWNER}"
owner="${2:?usage: check-public-packages.sh VERSION OWNER}"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf 'release version is not semantic: %s\n' "${version}" >&2
  exit 2
fi
if [[ ! "${owner}" =~ ^[0-9A-Za-z_.-]+$ ]]; then
  printf 'package owner is invalid: %s\n' "${owner}" >&2
  exit 2
fi

docker_bin="${DOCKER_BIN:-docker}"
helm_bin="${HELM_BIN:-helm}"
work_directory="$(mktemp -d "${TMPDIR:-/tmp}/suxen-public-packages.XXXXXX")"
trap 'rm -rf "${work_directory}"' EXIT
mkdir -p "${work_directory}/docker" "${work_directory}/buildx" "${work_directory}/helm"
printf '{}\n' >"${work_directory}/docker/config.json"

# Empty, isolated credential stores make this an anonymous read even when a
# preceding publication step logged Docker and Helm in to GHCR.
export DOCKER_CONFIG="${work_directory}/docker"
export BUILDX_CONFIG="${work_directory}/buildx"
export HELM_REGISTRY_CONFIG="${work_directory}/helm/registry.json"

for package in suxen suxen-sdk; do
  "${docker_bin}" buildx imagetools inspect --raw \
    "ghcr.io/${owner}/${package}:${version}" >/dev/null
done
"${helm_bin}" show chart "oci://ghcr.io/${owner}/suxen-chart" \
  --version "${version}" >/dev/null
