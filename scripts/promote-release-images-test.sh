#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT
calls="${work_directory}/calls"
manifest='{"manifests":[],"mediaType":"application/vnd.oci.image.index.v1+json","schemaVersion":2}'
digest="sha256:$(printf '%s' "${manifest}" | sha256sum | cut -d ' ' -f 1)"

cat >"${work_directory}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${PROMOTION_CALLS}"
if [[ "$*" == *"imagetools inspect --raw"* ]]; then
  printf '%s' "${PROMOTION_MANIFEST}"
fi
EOF
chmod +x "${work_directory}/docker"

PROMOTION_CALLS="${calls}" PROMOTION_MANIFEST="${manifest}" \
  DOCKER_BIN="${work_directory}/docker" \
  "${root_dir}/scripts/promote-release-images.sh" suxen-project "${digest}" "${digest}"

[[ "$(wc -l <"${calls}")" -eq 6 ]]
grep -Fqx \
  "buildx imagetools create --tag ghcr.io/suxen-project/suxen:latest ghcr.io/suxen-project/suxen@${digest}" \
  "${calls}"
grep -Fqx \
  "buildx imagetools create --tag ghcr.io/suxen-project/suxen-sdk:latest ghcr.io/suxen-project/suxen-sdk@${digest}" \
  "${calls}"

wrong_digest="sha256:$(printf '%064d' 0)"
if PROMOTION_CALLS="${calls}" PROMOTION_MANIFEST="${manifest}" \
  DOCKER_BIN="${work_directory}/docker" \
  "${root_dir}/scripts/promote-release-images.sh" suxen-project "${wrong_digest}" "${digest}" \
  >/dev/null 2>&1; then
  printf 'promotion accepted a latest manifest with the wrong digest\n' >&2
  exit 1
fi
