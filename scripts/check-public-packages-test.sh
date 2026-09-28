#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT
calls="${work_directory}/calls"

cat >"${work_directory}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "buildx imagetools inspect --raw ghcr.io/suxen-project/"*":1.0.0-rc.1" ]]
[[ "${DOCKER_CONFIG}" == */docker ]]
[[ "${BUILDX_CONFIG}" == */buildx ]]
[[ "$(cat "${DOCKER_CONFIG}/config.json")" == '{}' ]]
printf 'docker %s\n' "$*" >>"${CHECK_PUBLIC_CALLS}"
[[ "${FAKE_PUBLIC_FAILURE:-}" != docker ]]
EOF
cat >"${work_directory}/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "show chart oci://ghcr.io/suxen-project/suxen-chart --version 1.0.0-rc.1" ]]
[[ "${HELM_REGISTRY_CONFIG}" == */helm/registry.json ]]
[[ ! -e "${HELM_REGISTRY_CONFIG}" ]]
printf 'helm %s\n' "$*" >>"${CHECK_PUBLIC_CALLS}"
[[ "${FAKE_PUBLIC_FAILURE:-}" != helm ]]
EOF
chmod +x "${work_directory}/docker" "${work_directory}/helm"

CHECK_PUBLIC_CALLS="${calls}" \
  DOCKER_BIN="${work_directory}/docker" \
  HELM_BIN="${work_directory}/helm" \
  "${root_dir}/scripts/check-public-packages.sh" 1.0.0-rc.1 suxen-project

[[ "$(grep -c '^docker ' "${calls}")" -eq 2 ]]
grep -q 'ghcr.io/suxen-project/suxen:1.0.0-rc.1' "${calls}"
grep -q 'ghcr.io/suxen-project/suxen-sdk:1.0.0-rc.1' "${calls}"
grep -q '^helm show chart oci://ghcr.io/suxen-project/suxen-chart --version 1.0.0-rc.1$' "${calls}"

for failure in docker helm; do
  if CHECK_PUBLIC_CALLS="${calls}" \
    FAKE_PUBLIC_FAILURE="${failure}" \
    DOCKER_BIN="${work_directory}/docker" \
    HELM_BIN="${work_directory}/helm" \
    "${root_dir}/scripts/check-public-packages.sh" 1.0.0-rc.1 suxen-project \
    >/dev/null 2>&1; then
    printf 'anonymous %s failure was ignored\n' "${failure}" >&2
    exit 1
  fi
done
