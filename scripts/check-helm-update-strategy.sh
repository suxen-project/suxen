#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root_dir}/charts/suxen"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

render_strategy() {
  local output="$1"
  shift
  helm template rollout-review "${chart}" \
    --show-only templates/deployment.yaml "$@" >"${output}"
}

assert_strategy() {
  local output="$1"
  local expected="$2"
  if ! grep -A1 '^  strategy:$' "${output}" | grep -q "^    type: ${expected}$"; then
    printf 'Helm update strategy is not %s:\n' "${expected}" >&2
    sed -n '/^  strategy:/,+2p' "${output}" >&2
    exit 1
  fi
}

render_strategy "${work_dir}/rwop.yaml" \
  --set 'persistence.accessModes[0]=ReadWriteOncePod'
assert_strategy "${work_dir}/rwop.yaml" Recreate

render_strategy "${work_dir}/rwo.yaml"
assert_strategy "${work_dir}/rwo.yaml" Recreate

render_strategy "${work_dir}/shared.yaml" \
  --set persistence.enabled=false \
  --set database.url=postgres://database.example/suxen \
  --set blobStore.url=s3://release-bucket \
  --set bootstrap.existingSecret=suxen-bootstrap \
  --set config.cluster=true \
  --set replicaCount=3
assert_strategy "${work_dir}/shared.yaml" RollingUpdate
