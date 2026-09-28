#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: verify-published-release.sh VERSION [OWNER/REPOSITORY]}"
repository="${2:-suxen-project/suxen}"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf 'release version is not semantic: %s\n' "${version}" >&2
  exit 1
fi
if [[ ! "${repository}" =~ ^[0-9A-Za-z_.-]+/[0-9A-Za-z_.-]+$ ]]; then
  printf 'repository must be OWNER/NAME: %s\n' "${repository}" >&2
  exit 1
fi

for tool in cosign curl docker gh go helm jq kubectl sha256sum tar unzip; do
  if ! command -v "${tool}" >/dev/null; then
    printf 'required release verification tool is missing: %s\n' "${tool}" >&2
    exit 1
  fi
done

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d "${TMPDIR:-/tmp}/suxen-published-release.XXXXXX")"
artifacts_directory="${work_directory}/artifacts"
mkdir -p "${artifacts_directory}"
verification_id="$(date -u +%Y%m%d%H%M%S)-$$-${RANDOM}"
release_name="suxen-verify-${verification_id}"
namespace="${release_name}"
namespace_owned=false
cleanup() {
	# The namespace is unique to this invocation and is marked owned only after
	# its creation succeeds. Deleting it cleans a partially failed Helm install
	# without ever touching a pre-existing or concurrent verifier's release.
  if [[ "${namespace_owned}" == true ]]; then
		kubectl delete namespace "${namespace}" \
			--ignore-not-found=true --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "${work_directory}"
}
trap cleanup EXIT

source "${repository_root}/scripts/release-assets.sh"
mapfile -t assets < <(release_asset_names "${version}")
release_url="https://github.com/${repository}/releases/download/v${version}"
curl_options=(--fail --location --retry 5 --retry-all-errors --silent --show-error)
for asset in "${assets[@]}"; do
  curl "${curl_options[@]}" --output "${artifacts_directory}/${asset}" \
    "${release_url}/${asset}"
done
bundle="${work_directory}/SHA256SUMS.sigstore.json"
curl "${curl_options[@]}" --output "${bundle}" \
  "${release_url}/SHA256SUMS.sigstore.json"

"${repository_root}/scripts/verify-release-downloads.sh" \
  "${version}" "${repository}" "${artifacts_directory}" "${bundle}"

version_pattern="$(sed 's/\./\\./g' <<<"${version}")"
identity_regexp="${SUXEN_RELEASE_CERTIFICATE_IDENTITY_REGEXP:-^https://github.com/${repository}/\.github/workflows/ci\.yml@refs/tags/v${version_pattern}$}"
issuer="https://token.actions.githubusercontent.com"

attestation_policy=(
  --repo "${repository}"
  --signer-workflow "${repository}/.github/workflows/ci.yml"
  --source-ref "refs/tags/v${version}"
)

owner="${repository%%/*}"
runtime_image="ghcr.io/${owner}/suxen:${version}"
sdk_image="ghcr.io/${owner}/suxen-sdk:${version}"
chart="ghcr.io/${owner}/suxen-chart:${version}"
for reference in "${runtime_image}" "${sdk_image}"; do
  manifest="$(docker buildx imagetools inspect --raw "${reference}")"
  if ! jq -e \
    '[.manifests[] | select(.platform.os == "linux") | .platform.architecture] |
     ((index("amd64") != null) and (index("arm64") != null))' \
    <<<"${manifest}" >/dev/null; then
    printf '%s does not publish both linux/amd64 and linux/arm64\n' "${reference}" >&2
    exit 1
  fi
done

for reference in "${runtime_image}" "${sdk_image}" "${chart}"; do
  cosign verify \
    --certificate-identity-regexp "${identity_regexp}" \
    --certificate-oidc-issuer "${issuer}" \
    "${reference}" >/dev/null
  gh attestation verify "oci://${reference}" "${attestation_policy[@]}"
done

# The release must be usable without the workflow's registry credentials.
docker logout ghcr.io >/dev/null 2>&1 || true
helm registry logout ghcr.io >/dev/null 2>&1 || true
for platform in linux/amd64 linux/arm64; do
  platform_label="${platform//\//-}"
  docker pull --platform "${platform}" "${runtime_image}"
  "${repository_root}/scripts/smoke-runtime-image.sh" \
    "${runtime_image}" "${platform}" "${version}"
  docker pull --platform "${platform}" "${sdk_image}"
  SUXEN_SDK_SMOKE_TAG="published-${version}-${platform_label}" \
    "${repository_root}/scripts/smoke-sdk-image.sh" \
    "${sdk_image}" "${platform}" "${version}"
done

kubectl create namespace "${namespace}"
namespace_owned=true
kubectl label namespace "${namespace}" \
  app.kubernetes.io/managed-by=suxen-release-verifier \
  "suxen-project.io/verification-id=${verification_id}"
helm install "${release_name}" "oci://ghcr.io/${owner}/suxen-chart" \
  --version "${version}" \
  --namespace "${namespace}" \
  --set persistence.enabled=false \
  --wait \
  --timeout 5m
deployment="${release_name}-suxen"
kubectl rollout status "deployment/${deployment}" \
  --namespace "${namespace}" --timeout=2m
deployed_image="$(kubectl get "deployment/${deployment}" --namespace "${namespace}" \
  --output jsonpath='{.spec.template.spec.containers[0].image}')"
if [[ "${deployed_image}" != "${runtime_image}" ]]; then
  printf 'installed chart uses image %s, want %s\n' "${deployed_image}" "${runtime_image}" >&2
  exit 1
fi
installed_version="$(kubectl exec "deployment/${deployment}" --namespace "${namespace}" -- \
  /usr/local/bin/suxen version)"
if [[ "${installed_version}" != "${version}" ]]; then
  printf 'installed chart runs version %s, want %s\n' "${installed_version}" "${version}" >&2
  exit 1
fi
