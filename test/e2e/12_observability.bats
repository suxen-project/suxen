#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
  compose start postgres minio >/dev/null
  wait_until "dependencies before observability test" 45 service_ready suxen-b
}

teardown() {
  compose start postgres minio >/dev/null 2>&1 || true
}

direct_readiness_status() {
  local node_url
  node_url="$(current_service_url suxen-b)"
  curl \
    --max-time 5 \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    "${node_url}/readyz"
}

readiness_is_unavailable() {
  [[ "$(direct_readiness_status)" == "503" ]]
}

@test "health, readiness, and version are machine-readable" {
  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/healthz"
  assert_success
  run jq -er '.status == "ok"' <<<"${output}"
  assert_success

  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/readyz"
  assert_success
  run jq -er '.status == "ready"' <<<"${output}"
  assert_success

  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/version"
  assert_success
  run jq -er '.version == "e2e"' <<<"${output}"
  assert_success

}

@test "Prometheus exposition passes promtool validation" {
  require_command promtool

  run curl \
    --fail \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    "${SUXEN_E2E_URL}/metrics"
  assert_success
  run promtool check metrics <<<"${output}"
  assert_success
}

@test "readiness fails while PostgreSQL is unavailable without exposing health" {
  compose stop postgres

  wait_until "readiness failure with PostgreSQL stopped" 20 readiness_is_unavailable
  local node_url
  node_url="$(current_service_url suxen-b)"
  run curl --silent --output /dev/null --write-out '%{http_code}' "${node_url}/healthz"
  assert_success
  assert_equal "${output}" "200"

  compose start postgres
  wait_until "readiness after PostgreSQL restart" 45 \
    service_ready suxen-b
}

@test "readiness stays ready while MinIO is unavailable" {
  # Readiness gates only on metadata; a blob-store outage must not remove the
  # pod from rotation, since every replica shares the same external stores and
  # failing readiness on a store blip would drop them all at once.
  compose stop minio

  # Probe repeatedly across the outage window: readiness must never flip to 503.
  local attempt
  for attempt in 1 2 3 4 5; do
    run direct_readiness_status
    assert_success
    assert_equal "${output}" "200"
    sleep 1
  done

  local node_url
  node_url="$(current_service_url suxen-b)"
  run curl --silent --output /dev/null --write-out '%{http_code}' "${node_url}/healthz"
  assert_success
  assert_equal "${output}" "200"

  compose start minio
  wait_until "readiness after MinIO restart" 45 \
    service_ready suxen-b
}
