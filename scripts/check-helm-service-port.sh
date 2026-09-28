#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root_dir}/charts/suxen"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fail() {
  printf 'Helm service-port assertion failed: %s\n' "$*" >&2
  exit 1
}

helm template review "${chart}" \
  --set service.port=8081 \
  --show-only templates/service.yaml >"${work_dir}/service.yaml"
helm template review "${chart}" \
  --set service.port=8081 \
  --show-only templates/deployment.yaml >"${work_dir}/deployment.yaml"

grep -q '^      port: 8081$' "${work_dir}/service.yaml" ||
  fail "nondefault Service port was not rendered"
grep -q '^      targetPort: http$' "${work_dir}/service.yaml" ||
  fail "primary Service port does not target the named pod port"
grep -q '^              value: ":8080"$' "${work_dir}/deployment.yaml" ||
  fail "pod listener did not remain on 8080"
grep -q '^              containerPort: 8080$' "${work_dir}/deployment.yaml" ||
  fail "named pod port did not remain on 8080"
if grep -q 'containerPort: 8081' "${work_dir}/deployment.yaml"; then
  fail "external Service port leaked into the pod port"
fi
if [[ "$(grep -c '^              port: http$' "${work_dir}/deployment.yaml")" -ne 3 ]]; then
  fail "startup, liveness, and readiness probes do not all use the named pod port"
fi

# An extra OCI listener cannot share the external Service port: Kubernetes
# would otherwise receive two Service entries with the same port but different
# pod targets.
if helm template review "${chart}" \
  --set service.port=8081 \
  --set 'service.extraPorts[0].name=oci-conflict' \
  --set 'service.extraPorts[0].port=8081' \
  --set 'service.extraPorts[0].targetPort=8081' \
  >"${work_dir}/conflict.yaml" 2>"${work_dir}/conflict.err"; then
  fail "conflicting primary and OCI Service ports were accepted"
fi
grep -q 'OCI extra Service port 8081 conflicts with service.port' "${work_dir}/conflict.err" ||
  fail "conflicting Service port did not return the expected validation error"
