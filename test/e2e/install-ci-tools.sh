#!/usr/bin/env bash
set -euo pipefail

destination="${1:?usage: install-ci-tools.sh DESTINATION}"
architecture="$(uname -m)"
case "${architecture}" in
  x86_64)
    tool_architecture=amd64
    crane_architecture=x86_64
    ;;
  aarch64 | arm64)
    tool_architecture=arm64
    crane_architecture=arm64
    ;;
  *) printf 'unsupported architecture: %s\n' "${architecture}" >&2; exit 1 ;;
esac

# renovate: datasource=github-releases depName=bats-core/bats-core
BATS_VERSION=1.12.0
BATS_ARCHIVE_SHA256=e36b020436228262731e3319ed013d84fcd7c4bd97a1b34dee33d170e9ae6bab
# renovate: datasource=github-releases depName=sigstore/cosign
COSIGN_VERSION=2.4.1
# renovate: datasource=github-releases depName=google/go-containerregistry
CRANE_VERSION=0.20.3
# renovate: datasource=github-releases depName=gradle/gradle
GRADLE_VERSION=8.14.3
# renovate: datasource=github-releases depName=apache/maven extractVersion=^maven-(?<version>.*)$
MAVEN_VERSION=3.9.11
# renovate: datasource=github-releases depName=oras-project/oras
ORAS_VERSION=1.2.2
# renovate: datasource=github-releases depName=prometheus/prometheus
PROMETHEUS_VERSION=3.2.1

mkdir -p "${destination}/bin" "${destination}/downloads"
downloads="${destination}/downloads"

download() {
  local url="$1"
  local output="$2"
  printf 'Downloading %s\n' "${url}" >&2
  curl --fail --location --silent --show-error \
    --retry 3 --retry-delay 2 --retry-max-time 60 \
    --connect-timeout 15 --max-time 120 \
    "${url}" --output "${output}"
}

verify_checksum_file() {
  local checksum_file="$1"
  local asset="$2"
  local expected
  expected="$(awk -v name="$(basename "${asset}")" '$2 == name || $2 == "*" name {print $1; exit}' "${checksum_file}")"
  if [[ -z "${expected}" ]]; then
    printf 'no checksum found for %s in %s\n' "${asset}" "${checksum_file}" >&2
    return 1
  fi
  printf '%s  %s\n' "${expected}" "${asset}" | sha256sum --check --status
}

verify_sha256() {
  local expected="$1"
  local asset="$2"
  printf '%s  %s\n' "${expected}" "${asset}" | sha256sum --check --status
}

verify_downloaded_checksum() {
  local algorithm="$1"
  local checksum_file="$2"
  local asset="$3"
  local expected
  expected="$(awk 'NR == 1 {print $1}' "${checksum_file}")"
  if [[ -z "${expected}" ]]; then
    printf 'empty checksum file: %s\n' "${checksum_file}" >&2
    return 1
  fi
  printf '%s  %s\n' "${expected}" "${asset}" | "${algorithm}sum" --check --status
}

bats_archive="${downloads}/bats-core-v${BATS_VERSION}.tar.gz"
download \
  "https://github.com/bats-core/bats-core/archive/refs/tags/v${BATS_VERSION}.tar.gz" \
  "${bats_archive}"
verify_sha256 "${BATS_ARCHIVE_SHA256}" "${bats_archive}"
tar -xzf "${bats_archive}" -C "${downloads}"
"${downloads}/bats-core-${BATS_VERSION}/install.sh" "${destination}/bats"
ln -s "${destination}/bats/bin/bats" "${destination}/bin/bats"

oras_archive="${downloads}/oras_${ORAS_VERSION}_linux_${tool_architecture}.tar.gz"
download \
  "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/$(basename "${oras_archive}")" \
  "${oras_archive}"
download \
  "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_checksums.txt" \
  "${downloads}/oras-checksums.txt"
verify_checksum_file "${downloads}/oras-checksums.txt" "${oras_archive}"
tar -xzf "${oras_archive}" -C "${destination}/bin" oras

cosign_binary="${downloads}/cosign-linux-${tool_architecture}"
download \
  "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-${tool_architecture}" \
  "${cosign_binary}"
download \
  "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign_checksums.txt" \
  "${downloads}/cosign-checksums.txt"
verify_checksum_file "${downloads}/cosign-checksums.txt" "${cosign_binary}"
install -m 0755 "${cosign_binary}" "${destination}/bin/cosign"

maven_archive="${downloads}/apache-maven-${MAVEN_VERSION}-bin.tar.gz"
download \
  "https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/binaries/$(basename "${maven_archive}")" \
  "${maven_archive}"
download \
  "https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/binaries/$(basename "${maven_archive}").sha512" \
  "${maven_archive}.sha512"
verify_downloaded_checksum sha512 "${maven_archive}.sha512" "${maven_archive}"
tar -xzf "${maven_archive}" -C "${downloads}"
ln -s "${downloads}/apache-maven-${MAVEN_VERSION}/bin/mvn" "${destination}/bin/mvn"

gradle_archive="${downloads}/gradle-${GRADLE_VERSION}-bin.zip"
download \
  "https://services.gradle.org/distributions/$(basename "${gradle_archive}")" \
  "${gradle_archive}"
download \
  "https://services.gradle.org/distributions/$(basename "${gradle_archive}").sha256" \
  "${gradle_archive}.sha256"
verify_downloaded_checksum sha256 "${gradle_archive}.sha256" "${gradle_archive}"
unzip -q "${gradle_archive}" -d "${downloads}"
ln -s "${downloads}/gradle-${GRADLE_VERSION}/bin/gradle" "${destination}/bin/gradle"

crane_archive="${downloads}/go-containerregistry_Linux_${crane_architecture}.tar.gz"
if download \
  "https://github.com/google/go-containerregistry/releases/download/v${CRANE_VERSION}/$(basename "${crane_archive}")" \
  "${crane_archive}" && \
  download \
    "https://github.com/google/go-containerregistry/releases/download/v${CRANE_VERSION}/checksums.txt" \
    "${downloads}/crane-checksums.txt"; then
  verify_checksum_file "${downloads}/crane-checksums.txt" "${crane_archive}"
  tar -xzf "${crane_archive}" -C "${destination}/bin" crane
else
  # The release asset CDN can return a persistent 504. Go verifies the pinned
  # module against its checksum database before building the same crane version.
  rm -f "${crane_archive}"
  printf 'Building crane v%s from its pinned Go module\n' "${CRANE_VERSION}" >&2
  GOBIN="${destination}/bin" go install \
    "github.com/google/go-containerregistry/cmd/crane@v${CRANE_VERSION}"
fi

prometheus_archive="${downloads}/prometheus-${PROMETHEUS_VERSION}.linux-${tool_architecture}.tar.gz"
download \
  "https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/$(basename "${prometheus_archive}")" \
  "${prometheus_archive}"
download \
  "https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/sha256sums.txt" \
  "${downloads}/prometheus-checksums.txt"
verify_checksum_file "${downloads}/prometheus-checksums.txt" "${prometheus_archive}"
tar -xzf "${prometheus_archive}" -C "${downloads}"
install -m 0755 \
  "${downloads}/prometheus-${PROMETHEUS_VERSION}.linux-${tool_architecture}/promtool" \
  "${destination}/bin/promtool"

printf '%s\n' "${destination}/bin"
