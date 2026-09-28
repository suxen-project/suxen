#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "an asset written through replica A is readable through replica B" {
  printf 'cross-replica payload\n' >"${E2E_CASE_DIRECTORY}/cross-replica.txt"

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --upload-file "${E2E_CASE_DIRECTORY}/cross-replica.txt" \
    "${SUXEN_E2E_NODE_A}/repository/raw/cluster/cross-replica.txt"
  assert_success

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --output "${E2E_CASE_DIRECTORY}/from-b.txt" \
    "${SUXEN_E2E_NODE_B}/repository/raw/cluster/cross-replica.txt"
  assert_success
  run cmp "${E2E_CASE_DIRECTORY}/cross-replica.txt" "${E2E_CASE_DIRECTORY}/from-b.txt"
  assert_success
}

@test "the load balancer continues serving after one replica fails" {
  compose stop suxen-a

  run raw_get raw cluster/cross-replica.txt "${E2E_CASE_DIRECTORY}/after-failure.txt"
  assert_success
  run grep -Fx 'cross-replica payload' "${E2E_CASE_DIRECTORY}/after-failure.txt"
  assert_success

  compose start suxen-a
  wait_until "replica A readiness" 45 service_ready suxen-a
  compose exec -T load-balancer nginx -s reload
}

@test "concurrent same-digest uploads create one S3 object" {
  printf 'concurrent content-addressed payload\n' >"${E2E_CASE_DIRECTORY}/concurrent.txt"
  local before
  before="$(minio_object_count)"

  local upload
  local -a upload_pids=()
  for upload in $(seq 1 12); do
    curl \
      --fail-with-body \
      --silent \
      --show-error \
      --output /dev/null \
      --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
      --upload-file "${E2E_CASE_DIRECTORY}/concurrent.txt" \
      "${SUXEN_E2E_URL}/repository/raw/cluster/concurrent-${upload}.txt" &
    upload_pids+=("$!")
  done

  local upload_failed=0
  local upload_pid
  for upload_pid in "${upload_pids[@]}"; do
    if ! wait "${upload_pid}"; then
      printf 'concurrent upload process %s failed\n' "${upload_pid}" >&2
      upload_failed=1
    fi
  done
  if ((upload_failed != 0)); then
    fail "one or more concurrent uploads failed"
  fi

  for upload in $(seq 1 12); do
    local downloaded="${E2E_CASE_DIRECTORY}/concurrent-${upload}.txt"
    raw_get raw "cluster/concurrent-${upload}.txt" "${downloaded}"
    cmp "${E2E_CASE_DIRECTORY}/concurrent.txt" "${downloaded}"
  done

  local after
  after="$(minio_object_count)"
  assert_equal "$((after - before))" "1"
}

@test "all schedulers yield to one active leader and a new holder takes over" {
  local original_holder
  original_holder="$(api GET /api/v1/tasks/leader | jq -er '.lease.holder')"

  compose stop suxen-a suxen-b suxen-c
  # Expire the persisted lease directly instead of waiting out the 30s lease by
  # wall clock; production lease and renewal periods are unchanged.
  compose exec -T postgres psql \
    --username suxen \
    --dbname suxen \
    --command "UPDATE leader_leases SET expires_at = '1970-01-01T00:00:00Z';"
  compose start suxen-b suxen-c
  wait_until "replica B readiness after leader expiry" 45 service_ready suxen-b

  holder_changed() {
    local current
    current="$(curl \
      --fail \
      --silent \
      --show-error \
      --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
      "$(current_service_url suxen-b)/api/v1/tasks/leader" |
      jq -er '.lease.holder')" || return 1
    [[ "${current}" != "${original_holder}" ]]
  }
  wait_until "cleanup scheduler leader takeover" 20 holder_changed

  compose start suxen-a
  wait_until "replica A readiness after leader test" 45 service_ready suxen-a
  compose exec -T load-balancer nginx -s reload
}

@test "three fresh processes race one PostgreSQL migration safely" {
  compose exec -T postgres psql \
    --username suxen \
    --dbname postgres \
    --command 'DROP DATABASE IF EXISTS suxen_race WITH (FORCE);'
  compose exec -T postgres psql \
    --username suxen \
    --dbname postgres \
    --command 'CREATE DATABASE suxen_race;'

  run compose --profile migration-race up \
    --detach \
    --wait \
    migration-a migration-b migration-c
  assert_success

  run compose exec -T postgres psql \
    --username suxen \
    --dbname suxen_race \
    --tuples-only \
    --no-align \
    --command 'SELECT COUNT(*) FROM schema_migrations;'
  assert_success
  if [[ "${output}" -lt 1 ]]; then
    fail "fresh database did not record any schema migrations"
  fi

  compose --profile migration-race stop migration-a migration-b migration-c
}
