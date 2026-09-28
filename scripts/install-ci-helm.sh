#!/usr/bin/env bash
set -euo pipefail

destination="${1:?usage: install-ci-helm.sh DESTINATION}"
# renovate: datasource=github-releases depName=helm/helm
version=v3.16.4
archive="helm-${version}-linux-amd64.tar.gz"
checksum=fc307327959aa38ed8f9f7e66d45492bb022a66c3e5da6063958254b9767d179
work_directory="$(mktemp -d "${TMPDIR:-/tmp}/suxen-helm.XXXXXX")"
trap 'rm -rf "${work_directory}"' EXIT

curl --fail --silent --show-error --location \
  --output "${work_directory}/${archive}" \
  "https://get.helm.sh/${archive}"
printf '%s  %s\n' "${checksum}" "${work_directory}/${archive}" | sha256sum --check --status
tar -C "${work_directory}" -xzf "${work_directory}/${archive}"
mkdir -p "${destination}"
install -m 0755 "${work_directory}/linux-amd64/helm" "${destination}/helm"
