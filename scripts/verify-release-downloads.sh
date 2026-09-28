#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: verify-release-downloads.sh VERSION OWNER/REPOSITORY ARTIFACTS_DIRECTORY BUNDLE}"
repository="${2:?usage: verify-release-downloads.sh VERSION OWNER/REPOSITORY ARTIFACTS_DIRECTORY BUNDLE}"
artifacts_directory="${3:?usage: verify-release-downloads.sh VERSION OWNER/REPOSITORY ARTIFACTS_DIRECTORY BUNDLE}"
bundle="${4:?usage: verify-release-downloads.sh VERSION OWNER/REPOSITORY ARTIFACTS_DIRECTORY BUNDLE}"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf 'release version is not semantic: %s\n' "${version}" >&2
  exit 2
fi
if [[ ! "${repository}" =~ ^[0-9A-Za-z_.-]+/[0-9A-Za-z_.-]+$ ]]; then
  printf 'repository must be OWNER/NAME: %s\n' "${repository}" >&2
  exit 2
fi

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cosign_bin="${COSIGN_BIN:-cosign}"
gh_bin="${GH_BIN:-gh}"
artifact_checker="${SUXEN_RELEASE_ARTIFACT_CHECKER:-${repository_root}/scripts/check-release-artifacts.sh}"
source "${repository_root}/scripts/release-assets.sh"
mapfile -t assets < <(release_asset_names "${version}")

version_pattern="$(sed 's/\./\\./g' <<<"${version}")"
identity_regexp="${SUXEN_RELEASE_CERTIFICATE_IDENTITY_REGEXP:-^https://github.com/${repository}/\.github/workflows/ci\.yml@refs/tags/v${version_pattern}$}"
issuer="https://token.actions.githubusercontent.com"
"${cosign_bin}" verify-blob \
  --bundle "${bundle}" \
  --certificate-identity-regexp "${identity_regexp}" \
  --certificate-oidc-issuer "${issuer}" \
  "${artifacts_directory}/SHA256SUMS"

attestation_policy=(
  --repo "${repository}"
  --signer-workflow "${repository}/.github/workflows/ci.yml"
  --source-ref "refs/tags/v${version}"
)
for asset in "${assets[@]}"; do
  "${gh_bin}" attestation verify \
    "${artifacts_directory}/${asset}" "${attestation_policy[@]}"
done

# The checker extracts archives and executes Linux binaries. Keep it after the
# signed checksum manifest and every downloaded artifact attestation succeed.
SUXEN_RELEASE_EXECUTE_LINUX_TARGETS="${SUXEN_RELEASE_EXECUTE_LINUX_TARGETS:-amd64,arm64}" \
  "${artifact_checker}" "${version}" "${artifacts_directory}"
