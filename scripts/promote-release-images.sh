#!/usr/bin/env bash
set -euo pipefail

owner="${1:?usage: promote-release-images.sh OWNER RUNTIME_DIGEST SDK_DIGEST}"
runtime_digest="${2:?usage: promote-release-images.sh OWNER RUNTIME_DIGEST SDK_DIGEST}"
sdk_digest="${3:?usage: promote-release-images.sh OWNER RUNTIME_DIGEST SDK_DIGEST}"
docker_bin="${DOCKER_BIN:-docker}"

if [[ ! "${owner}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
	printf 'invalid registry owner: %s\n' "${owner}" >&2
	exit 1
fi
for digest in "${runtime_digest}" "${sdk_digest}"; do
	if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
		printf 'invalid image digest: %s\n' "${digest}" >&2
		exit 1
	fi
done

manifest_digest() {
	local reference="$1"
	local raw_manifest
	local digest
	raw_manifest="$(mktemp)"
	if ! "${docker_bin}" buildx imagetools inspect --raw "${reference}" >"${raw_manifest}"; then
		rm -f "${raw_manifest}"
		return 1
	fi
	digest="sha256:$(sha256sum "${raw_manifest}" | cut -d ' ' -f 1)"
	rm -f "${raw_manifest}"
	printf '%s\n' "${digest}"
}

verify_source() {
	local image="$1"
	local expected="$2"
	local observed
	observed="$(manifest_digest "${image}@${expected}")"
	if [[ "${observed}" != "${expected}" ]]; then
		printf '%s@%s resolves to %s\n' "${image}" "${expected}" "${observed}" >&2
		exit 1
	fi
}

promote() {
	local image="$1"
	local digest="$2"
	local observed

	"${docker_bin}" buildx imagetools create \
		--tag "${image}:latest" "${image}@${digest}"
	observed="$(manifest_digest "${image}:latest")"
	if [[ "${observed}" != "${digest}" ]]; then
		printf '%s:latest resolves to %s, expected %s\n' \
			"${image}" "${observed}" "${digest}" >&2
		exit 1
	fi
}

verify_source "ghcr.io/${owner}/suxen" "${runtime_digest}"
verify_source "ghcr.io/${owner}/suxen-sdk" "${sdk_digest}"
promote "ghcr.io/${owner}/suxen" "${runtime_digest}"
promote "ghcr.io/${owner}/suxen-sdk" "${sdk_digest}"
