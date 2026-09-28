#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "credentialed Raw proxy caches one upstream fetch and redacts credentials" {
  printf 'credentialed proxy payload\n' >"${E2E_CASE_DIRECTORY}/upstream.txt"
  curl \
    --fail \
    --silent \
    --show-error \
    --user upstream:upstream-password \
    --upload-file "${E2E_CASE_DIRECTORY}/upstream.txt" \
    "${SUXEN_E2E_RAW_UPSTREAM}/files/releases/upstream.txt"
  curl --fail --silent --show-error --request DELETE "${SUXEN_E2E_RAW_UPSTREAM}/__hits"

  raw_get raw-proxy releases/upstream.txt "${E2E_CASE_DIRECTORY}/first.txt" "${SUXEN_E2E_ADMIN_TOKEN}"
  raw_get raw-proxy releases/upstream.txt "${E2E_CASE_DIRECTORY}/second.txt" "${SUXEN_E2E_ADMIN_TOKEN}"
  run cmp "${E2E_CASE_DIRECTORY}/upstream.txt" "${E2E_CASE_DIRECTORY}/second.txt"
  assert_success

  run curl --fail --silent --show-error \
    "${SUXEN_E2E_RAW_UPSTREAM}/__hits?path=releases%2Fupstream.txt"
  assert_success
  run jq -er '.count == 1' <<<"${output}"
  assert_success

  run api GET /api/v1/repositories/raw-proxy
  assert_success
  assert_contains "${output}" 'http://raw-upstream:8080/files'
  if [[ "${output}" == *"upstream-password"* ]]; then
    fail "repository API exposed upstream credentials"
  fi
}

@test "Raw group falls through to its proxy member and rejects writes" {
  run raw_get raw-group releases/upstream.txt "${E2E_CASE_DIRECTORY}/group.txt" "${SUXEN_E2E_ADMIN_TOKEN}"
  assert_success
  run grep -Fx 'credentialed proxy payload' "${E2E_CASE_DIRECTORY}/group.txt"
  assert_success

  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --request PUT \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --data 'not writable' \
    "${SUXEN_E2E_URL}/repository/raw-group/rejected.txt"
  assert_success
  assert_equal "${output}" "405"
}
