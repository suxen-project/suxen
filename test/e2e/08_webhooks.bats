#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "the real receiver HMAC-verifies delete events" {
  curl --fail --silent --show-error --request DELETE "${SUXEN_E2E_WEBHOOK}/events"
  printf 'webhook payload\n' >"${E2E_CASE_DIRECTORY}/event.txt"

  raw_put raw webhook/event.txt "${E2E_CASE_DIRECTORY}/event.txt" >/dev/null
  run api_status DELETE /repository/raw/webhook/event.txt "${SUXEN_E2E_ADMIN_TOKEN}"
  assert_success
  assert_equal "${output}" "204"

  events_delivered() {
    local events
    events="$(webhook_events)" || return 1
    jq -e 'any(.[]; .payload.type == "asset.deleted")' <<<"${events}" >/dev/null
  }
  wait_until "signed webhook delete delivery" 30 events_delivered
}

@test "a persistently failing receiver reaches dead-letter history" {
  local webhook='{
    "url":"http://webhook-sink:8080/fail",
    "events":["asset.uploaded"],
    "repositories":["raw"],
    "enabled":true
  }'
  # The chart provisions this webhook. This test intentionally takes ownership
  # before changing it so subsequent updates exercise the imperative lifecycle.
  run api PUT '/api/v1/webhooks/failing?force=true' "${webhook}"
  assert_success
  if [[ "${output}" == *"webhook-test-secret"* ]]; then
    fail "webhook update exposed its signing secret"
  fi

  printf 'dead-letter payload\n' >"${E2E_CASE_DIRECTORY}/dead.txt"
  raw_put raw webhook/dead.txt "${E2E_CASE_DIRECTORY}/dead.txt" >/dev/null
  # With the fast retry cadence configured in compose.e2e.yaml the 8 attempts
  # dead-letter within a few seconds; 30s is a generous ceiling for CI jitter.
  wait_until "webhook dead-letter transition" 30 \
    wait_for_delivery_status failing dead

  run api GET '/api/v1/webhooks/failing/deliveries?limit=200'
  assert_success
  run jq -e '
    .items |
    any(.status == "dead" and .attempts == 8 and (.lastError | contains("503")))
  ' <<<"${output}"
  assert_success

  webhook='{
    "url":"http://webhook-sink:8080/fail",
    "events":["asset.uploaded"],
    "repositories":["raw"],
    "enabled":false
  }'
  api PUT /api/v1/webhooks/failing "${webhook}" >/dev/null
}
