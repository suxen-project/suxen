# shellcheck shell=bash

load_e2e_environment() {
  if [[ -z "${SUXEN_E2E_RUNTIME_FILE:-}" ]]; then
    printf 'SUXEN_E2E_RUNTIME_FILE must be exported by test/e2e/run.sh\n' >&2
    return 1
  fi
  local runtime_file="${SUXEN_E2E_RUNTIME_FILE}"
  if [[ "${runtime_file}" != /* ]]; then
    printf 'E2E runtime file path must be absolute: %s\n' "${runtime_file}" >&2
    return 1
  fi
  if [[ ! -r "${runtime_file}" ]]; then
    printf 'missing E2E runtime file: %s\n' "${runtime_file}" >&2
    return 1
  fi
  # The runner creates this file from validated, locally generated values only.
  # shellcheck disable=SC1090
  source "${runtime_file}"
  export DOCKER_CONFIG SUXEN_E2E_ADMIN_TOKEN SUXEN_E2E_PROJECT SUXEN_E2E_REGISTRY SUXEN_E2E_URL
  export SUXEN_E2E_RAW_UPSTREAM SUXEN_E2E_UPSTREAM_REGISTRY SUXEN_E2E_WEBHOOK
  export SUXEN_E2E_NODE_A SUXEN_E2E_NODE_B SUXEN_E2E_NODE_C
  export SUXEN_E2E_CLIENT_HOST SUXEN_E2E_WRITE_REGISTRY
  export SUXEN_E2E_DOCKER_REGISTRY SUXEN_E2E_DOCKER_WRITE_REGISTRY
  export SUXEN_E2E_GRADLE_DIRECTORY
}

compose() {
  docker compose \
    --project-name "${SUXEN_E2E_PROJECT}" \
    --file "${BATS_TEST_DIRNAME}/compose.e2e.yaml" \
    "$@"
}

current_service_url() {
  local service="$1"
  local container_port="${2:-8080}"
  local address
  address="$(compose port "${service}" "${container_port}" | tail -n 1)"
  if [[ ! "${address}" =~ :([0-9]+)$ ]]; then
    printf 'could not resolve published port for %s: %s\n' "${service}" "${address}" >&2
    return 1
  fi
  printf 'http://%s:%s' "${SUXEN_E2E_CLIENT_HOST}" "${BASH_REMATCH[1]}"
}

service_ready() {
  local service="$1"
  compose exec -T raw-upstream \
    /usr/local/bin/e2e-support health "http://${service}:8080/readyz" >/dev/null 2>&1
}

api() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  local arguments=(
    --fail-with-body
    --silent
    --show-error
    --request "${method}"
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}"
  )
  if [[ -n "${body}" ]]; then
    arguments+=(--header "Content-Type: application/json" --data "${body}")
  fi
  curl "${arguments[@]}" "${SUXEN_E2E_URL}${path}"
}

api_if_match() {
  local method="$1"
  local path="$2"
  local digest="$3"
  local body="${4:-}"
  local arguments=(
    --fail-with-body
    --silent
    --show-error
    --request "${method}"
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}"
    --header "If-Match: ${digest}"
  )
  if [[ -n "${body}" ]]; then
    arguments+=(--header "Content-Type: application/json" --data "${body}")
  fi
  curl "${arguments[@]}" "${SUXEN_E2E_URL}${path}"
}

api_status() {
  local method="$1"
  local path="$2"
  # An omitted token uses the administrator default; an explicit empty token
  # intentionally performs an unauthenticated request.
  local token="${3-${SUXEN_E2E_ADMIN_TOKEN}}"
  local body="${4:-}"
  local arguments=(
    --silent
    --show-error
    --output /dev/null
    --write-out '%{http_code}'
    --request "${method}"
  )
  if [[ -n "${token}" ]]; then
    arguments+=(--header "Authorization: Bearer ${token}")
  fi
  if [[ -n "${body}" ]]; then
    arguments+=(--header "Content-Type: application/json" --data "${body}")
  fi
  curl "${arguments[@]}" "${SUXEN_E2E_URL}${path}"
}

raw_put() {
  local repository="$1"
  local path="$2"
  local source="$3"
  local token="${4:-${SUXEN_E2E_ADMIN_TOKEN}}"
  curl \
    --fail-with-body \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${token}" \
    --upload-file "${source}" \
    "${SUXEN_E2E_URL}/repository/${repository}/${path}"
}

raw_get() {
  local repository="$1"
  local path="$2"
  local destination="$3"
  # An omitted token uses the administrator default; an explicit empty token
  # intentionally performs an unauthenticated request.
  local token="${4-${SUXEN_E2E_ADMIN_TOKEN}}"
  local arguments=(--fail-with-body --silent --show-error --output "${destination}")
  if [[ -n "${token}" ]]; then
    arguments+=(--header "Authorization: Bearer ${token}")
  fi
  curl "${arguments[@]}" "${SUXEN_E2E_URL}/repository/${repository}/${path}"
}

asset_id() {
  local repository="$1"
  local path="$2"
  api GET "/api/v1/repositories/${repository}/assets?prefix=${path}" |
    jq --arg path "${path}" -er '.items[] | select(.path == $path) | .id'
}

asset_digest() {
  local repository="$1"
  local path="$2"
  api GET "/api/v1/repositories/${repository}/assets?prefix=${path}" |
    jq --arg path "${path}" -er '.items[] | select(.path == $path) | .digest'
}

webhook_events() {
  curl --fail --silent --show-error "${SUXEN_E2E_WEBHOOK}/events"
}

minio_object_count() {
  compose run --rm --no-deps --entrypoint /bin/sh minio-init -c \
    'mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc find local/suxen-e2e/blobs --json' \
    2>/dev/null |
    jq -s 'map(select(.status == "success" and .key != null)) | length'
}

registry_login() {
  local registry="${1:-${SUXEN_E2E_WRITE_REGISTRY}}"
  printf '%s' admin-password |
    docker login "${registry}" --username admin --password-stdin
}

wait_for_delivery_status() {
  local webhook="$1"
  local wanted="$2"
  local response
  response="$(api GET "/api/v1/webhooks/${webhook}/deliveries?limit=200")" || return 1
  jq -e --arg status "${wanted}" '.items | any(.status == $status)' <<<"${response}" >/dev/null
}
