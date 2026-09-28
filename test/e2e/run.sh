#!/usr/bin/env bash
set -euo pipefail
umask 077

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository_root="$(cd -- "${script_directory}/../.." && pwd)"
project_suffix="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
project_name="suxen-e2e-$(tr -cd 'a-zA-Z0-9-' <<<"${project_suffix}" | tr '[:upper:]' '[:lower:]')"
run_directory="$(mktemp -d "${TMPDIR:-/tmp}/${project_name}.XXXXXX")"
runtime_file="${run_directory}/runtime.env"
docker_config="${run_directory}/docker-config"
mkdir -p "${docker_config}"

admin_token="${SUXEN_E2E_ADMIN_TOKEN:-suxen-e2e-admin-token-value}"
export SUXEN_E2E_ADMIN_TOKEN="${admin_token}"

client_host="127.0.0.1"
publish_address="127.0.0.1"
# Containerized CI runs may execute clients in a job container while its Docker
# socket controls the host daemon. Published ports must therefore be reachable
# through the bridge gateway; local developer runs remain loopback-only.
if [[ -n "${HOSTNAME:-}" ]] && docker inspect "${HOSTNAME}" >/dev/null 2>&1; then
  detected_gateway="$(docker inspect "${HOSTNAME}" |
    jq -r '
      .[0].NetworkSettings.Networks
      | to_entries
      | map(.value.Gateway // empty)
      | map(select(length > 0))
      | first // empty
    ')"
  if [[ -n "${detected_gateway}" ]]; then
    client_host="${detected_gateway}"
    publish_address="0.0.0.0"
  fi
fi
export SUXEN_E2E_PUBLISH_ADDRESS="${publish_address}"

compose() {
  docker compose \
    --project-name "${project_name}" \
    --file "${script_directory}/compose.e2e.yaml" \
    "$@"
}

cleanup() {
  local exit_status=$?
  if [[ "${exit_status}" -ne 0 ]]; then
    compose ps || true
    compose logs --no-color --tail=300 || true
  fi
  compose --profile migration-race down --volumes --remove-orphans --rmi local || true
  rm -rf "${run_directory}"
  exit "${exit_status}"
}
trap cleanup EXIT INT TERM

host_port() {
  local service="$1"
  local container_port="$2"
  local address
  address="$(compose port "${service}" "${container_port}" | tail -n 1)"
  if [[ ! "${address}" =~ :([0-9]+)$ ]]; then
    printf 'could not parse published address for %s: %s\n' "${service}" "${address}" >&2
    return 1
  fi
  printf '%s' "${BASH_REMATCH[1]}"
}

cd "${repository_root}"
if ! compose config --format json |
  jq -e '[.services[] | .volumes[]? | select(.type == "bind")] | length == 0' >/dev/null; then
  printf 'E2E Compose topology must not contain host bind mounts\n' >&2
  exit 1
fi
compose build --pull suxen-a raw-upstream registry-auth webhook-sink load-balancer upstream-registry minio minio-init
compose up --detach --wait

suxen_port="$(host_port load-balancer 8080)"
registry_port="$(host_port upstream-registry 5000)"
upstream_port="$(host_port raw-upstream 8080)"
webhook_port="$(host_port webhook-sink 8080)"
node_a_port="$(host_port suxen-a 8080)"
node_b_port="$(host_port suxen-b 8080)"
node_c_port="$(host_port suxen-c 8080)"

cat >"${runtime_file}" <<EOF
DOCKER_CONFIG=${docker_config}
SUXEN_E2E_PROJECT=${project_name}
SUXEN_E2E_ADMIN_TOKEN=${admin_token}
SUXEN_E2E_CLIENT_HOST=${client_host}
SUXEN_E2E_NODE_A=http://${client_host}:${node_a_port}
SUXEN_E2E_NODE_B=http://${client_host}:${node_b_port}
SUXEN_E2E_NODE_C=http://${client_host}:${node_c_port}
SUXEN_E2E_RAW_UPSTREAM=http://${client_host}:${upstream_port}
SUXEN_E2E_REGISTRY=${client_host}:${suxen_port}
SUXEN_E2E_UPSTREAM_REGISTRY=${client_host}:${registry_port}
SUXEN_E2E_URL=http://${client_host}:${suxen_port}
SUXEN_E2E_WEBHOOK=http://${client_host}:${webhook_port}
SUXEN_E2E_WRITE_REGISTRY=${client_host}:${node_a_port}
SUXEN_E2E_DOCKER_REGISTRY=127.0.0.1:${suxen_port}
SUXEN_E2E_DOCKER_WRITE_REGISTRY=127.0.0.1:${node_a_port}
SUXEN_E2E_GRADLE_DIRECTORY=${run_directory}/gradle-client
EOF

export DOCKER_CONFIG="${docker_config}"
export SUXEN_E2E_RUNTIME_FILE="${runtime_file}"

mapfile -t test_files < <(find "${script_directory}" -maxdepth 1 -name '*.bats' -print | sort)
if [[ "${#test_files[@]}" -eq 0 ]]; then
  printf 'no Bats tests found in %s\n' "${script_directory}" >&2
  exit 1
fi

bats_output="${run_directory}/bats.tap"
bats \
  --formatter tap \
  --print-output-on-failure \
  --timing \
  "$@" \
  "${test_files[@]}" |
  tee "${bats_output}"

if [[ \
  "$#" -eq 0 && \
  "${SUXEN_E2E_FULL:-0}" == "1" \
]]; then
  expected_skips=0
  actual_skips="$(grep --count ' # skip ' "${bats_output}" || true)"
  if [[ "${actual_skips}" -ne "${expected_skips}" ]]; then
    printf \
      'full interoperability run requires %d skips, observed %d\n' \
      "${expected_skips}" \
      "${actual_skips}" >&2
    exit 1
  fi
fi
