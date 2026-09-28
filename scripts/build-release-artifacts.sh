#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: build-release-artifacts.sh VERSION [OUTPUT_DIRECTORY]}"
output_directory="${2:-dist}"

if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf 'release version is not semantic: %s\n' "${version}" >&2
  exit 1
fi

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
output_directory="$(mkdir -p "${output_directory}" && cd -- "${output_directory}" && pwd)"
work_directory="$(mktemp -d "${TMPDIR:-/tmp}/suxen-release.XXXXXX")"
trap 'rm -rf "${work_directory}"' EXIT

find "${output_directory}" -mindepth 1 -maxdepth 1 -type f -delete

targets=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
)

cd "${repository_root}"
for target in "${targets[@]}"; do
  os="${target%/*}"
  architecture="${target#*/}"
  suffix=""
  archive_extension="tar.gz"
  if [[ "${os}" == windows ]]; then
    suffix=".exe"
    archive_extension="zip"
  fi

  artifact_name="suxen-${version}-${os}-${architecture}"
  artifact_directory="${work_directory}/${artifact_name}"
  mkdir -p "${artifact_directory}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${architecture}" go build -trimpath \
    -ldflags="-s -w -X github.com/suxen-project/suxen/internal/server.Version=${version}" \
    -o "${artifact_directory}/suxen${suffix}" ./cmd/suxen
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${architecture}" go build -trimpath \
    -ldflags="-s -w -X main.version=${version}" \
    -o "${artifact_directory}/suxenctl${suffix}" ./cmd/suxenctl
  cp LICENSE NOTICE README.md "${artifact_directory}/"

  if [[ "${archive_extension}" == zip ]]; then
    (cd "${artifact_directory}" && zip -qr "${output_directory}/${artifact_name}.zip" .)
  else
    tar -C "${artifact_directory}" -czf "${output_directory}/${artifact_name}.tar.gz" .
  fi
done

helm package charts/suxen \
  --version "${version}" \
  --app-version "${version}" \
  --destination "${output_directory}"

(cd "${output_directory}" && sha256sum suxen-* >SHA256SUMS)
