#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "a fresh instance does not grant anonymous repository reads" {
  run api_status GET /api/v1/repositories/raw ""
  assert_success
  assert_equal "${output}" "401"

  run api_status GET /repository/raw/private/missing.txt ""
  assert_success
  assert_equal "${output}" "401"
}

@test "an HTTP-created role and user receive a scoped, immediately revocable token" {
  local role='{
    "name":"http-publisher",
    "description":"Created through the black-box API",
    "privileges":["repository:raw:read","repository:raw:write"]
  }'
  run api POST /api/v1/roles "${role}"
  assert_success
  run jq -e '
    .name == "http-publisher" and
    (.privileges | index("repository:raw:read")) and
    (.privileges | index("repository:raw:write"))
  ' <<<"${output}"
  assert_success

  local user='{
    "username":"http-publisher",
    "password":"http-publisher-password",
    "admin":false,
    "roles":["http-publisher"]
  }'
  run api POST /api/v1/users "${user}"
  assert_success
  run api GET /api/v1/users/http-publisher
  assert_success
  run jq -e '
    .username == "http-publisher" and
    .admin == false and
    (.roles == ["http-publisher"]) and
    (has("password") | not)
  ' <<<"${output}"
  assert_success

  run api POST '/api/v1/users/http-publisher/tokens' \
    '{"name":"e2e","scopes":["repository:raw:read","repository:raw:write"]}'
  assert_success
  local token
  token="$(jq -er '.token' <<<"${output}")"

  printf 'scoped token payload\n' >"${E2E_CASE_DIRECTORY}/scoped.txt"
  run raw_put raw auth/scoped.txt "${E2E_CASE_DIRECTORY}/scoped.txt" "${token}"
  assert_success

  run api_status DELETE /repository/raw/auth/scoped.txt "${token}"
  assert_success
  assert_equal "${output}" "403"

  run api GET /api/v1/users/http-publisher/tokens
  assert_success
  local token_id
  token_id="$(jq -er '.items[] | select(.name == "e2e") | .id' <<<"${output}")"
  run api DELETE "/api/v1/users/http-publisher/tokens/${token_id}"
  assert_success

  run api_status GET /api/v1/whoami "${token}"
  assert_success
  assert_equal "${output}" "401"
}

@test "whoami reports the authentication kind without returning credentials" {
  run curl \
    --fail \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    "${SUXEN_E2E_URL}/api/v1/whoami"
  assert_success
  run jq -e '
    .authenticated == true and
    .authenticationKind == "api-token" and
    .username == "admin" and
    (has("token") | not) and
    (has("password") | not)
  ' <<<"${output}"
  assert_success
}
