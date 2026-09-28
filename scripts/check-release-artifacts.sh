#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: check-release-artifacts.sh VERSION [OUTPUT_DIRECTORY]}"
output_directory="${2:-dist}"
output_directory="$(cd -- "${output_directory}" && pwd)"

expected=(
  SHA256SUMS
  "suxen-${version}-darwin-amd64.tar.gz"
  "suxen-${version}-darwin-arm64.tar.gz"
  "suxen-${version}-linux-amd64.tar.gz"
  "suxen-${version}-linux-arm64.tar.gz"
  "suxen-${version}-windows-amd64.zip"
  "suxen-${version}-windows-arm64.zip"
  "suxen-chart-${version}.tgz"
)
mapfile -t actual < <(find "${output_directory}" -maxdepth 1 -type f -printf '%f\n' | sort)
if [[ "$(printf '%s\n' "${actual[@]}")" != "$(printf '%s\n' "${expected[@]}" | sort)" ]]; then
  printf 'release artifact set does not match version %s\nexpected:\n' "${version}" >&2
  printf '  %s\n' "${expected[@]}" >&2
  printf 'actual:\n  %s\n' "${actual[@]}" >&2
  exit 1
fi

(cd "${output_directory}" && sha256sum --check --strict SHA256SUMS)

work_directory="$(mktemp -d "${TMPDIR:-/tmp}/suxen-release-check.XXXXXX")"
trap 'rm -rf "${work_directory}"' EXIT

check_binary() {
  local binary="$1"
  local os="$2"
  local architecture="$3"
  local metadata
  metadata="$(go version -m "${binary}")"
  if ! grep -Fq $'\tbuild\tGOOS='"${os}" <<<"${metadata}"; then
    printf '%s does not report GOOS=%s\n' "${binary}" "${os}" >&2
    exit 1
  fi
  if ! grep -Fq $'\tbuild\tGOARCH='"${architecture}" <<<"${metadata}"; then
    printf '%s does not report GOARCH=%s\n' "${binary}" "${architecture}" >&2
    exit 1
  fi
  if ! grep -aFq "${version}" "${binary}"; then
    printf '%s does not embed release version %s\n' "${binary}" "${version}" >&2
    exit 1
  fi
}

targets=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
)
for target in "${targets[@]}"; do
  os="${target%/*}"
  architecture="${target#*/}"
  suffix=""
  archive="${output_directory}/suxen-${version}-${os}-${architecture}.tar.gz"
  if [[ "${os}" == windows ]]; then
    suffix=".exe"
    archive="${output_directory}/suxen-${version}-${os}-${architecture}.zip"
  fi
  extracted="${work_directory}/${os}-${architecture}"
  mkdir -p "${extracted}"
  if [[ "${os}" == windows ]]; then
    unzip -q "${archive}" -d "${extracted}"
  else
    tar -C "${extracted}" -xzf "${archive}"
  fi
  for member in "suxen${suffix}" "suxenctl${suffix}" LICENSE NOTICE README.md; do
    if [[ ! -f "${extracted}/${member}" ]]; then
      printf '%s is missing %s\n' "${archive}" "${member}" >&2
      exit 1
    fi
  done
  check_binary "${extracted}/suxen${suffix}" "${os}" "${architecture}"
  check_binary "${extracted}/suxenctl${suffix}" "${os}" "${architecture}"
done

execution_targets="${SUXEN_RELEASE_EXECUTE_LINUX_TARGETS:-native}"
case "${execution_targets}" in
  native)
    case "$(uname -m)" in
      x86_64) execution_targets=amd64 ;;
      aarch64 | arm64) execution_targets=arm64 ;;
      *) printf 'unsupported native architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
    esac
    ;;
esac
IFS=',' read -r -a linux_architectures <<<"${execution_targets}"
for architecture in "${linux_architectures[@]}"; do
  case "${architecture}" in
    amd64 | arm64) ;;
    *) printf 'unsupported Linux execution target: %s\n' "${architecture}" >&2; exit 1 ;;
  esac
  extracted="${work_directory}/linux-${architecture}"
  if [[ "$("${extracted}/suxen" version)" != "${version}" ]]; then
    printf 'linux/%s server binary version does not match %s\n' \
      "${architecture}" "${version}" >&2
    exit 1
  fi
  if [[ "$("${extracted}/suxenctl" version)" != "${version}" ]]; then
    printf 'linux/%s client binary version does not match %s\n' \
      "${architecture}" "${version}" >&2
    exit 1
  fi
done

chart_metadata="$(helm show chart "${output_directory}/suxen-chart-${version}.tgz")"
chart_version="$(awk '/^version:/ {gsub(/"/, "", $2); print $2}' <<<"${chart_metadata}")"
application_version="$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2}' <<<"${chart_metadata}")"
if [[ "${chart_version}" != "${version}" || "${application_version}" != "${version}" ]]; then
  printf 'chart version=%s appVersion=%s, want %s\n' \
    "${chart_version}" "${application_version}" "${version}" >&2
  exit 1
fi
