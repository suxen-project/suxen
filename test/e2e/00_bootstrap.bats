#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "three replicas become ready behind the load balancer" {
  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/readyz"
  assert_success
  run jq -er '.status == "ready"' <<<"${output}"
  assert_success

  local node_url
  for node_url in "${SUXEN_E2E_NODE_A}" "${SUXEN_E2E_NODE_B}" "${SUXEN_E2E_NODE_C}"; do
    run curl --fail --silent --show-error "${node_url}/readyz"
    assert_success
  done
}

@test "startup provisioning created the black-box resources once" {
  run api GET /api/v1/repositories
  assert_success
  run jq -e '
    [.items[].name] as $names |
    [
      "maven-group",
      "maven-hosted",
      "maven-proxy",
      "oci",
      "oci-auth-proxy",
      "oci-group",
      "oci-proxy",
      "raw",
      "raw-group",
      "raw-proxy"
    ] |
    all(. as $name | $names | index($name))
  ' <<<"${output}"
  assert_success

  run api GET /api/v1/webhooks
  assert_success
  local webhook_response="${output}"
  run jq -e '[.items[].name] | index("events") and index("failing")' <<<"${output}"
  assert_success
  assert_contains "${webhook_response}" '"events"'
  if [[ "${webhook_response}" == *"webhook-test-secret"* ]]; then
    fail "webhook collection exposed its signing secret"
  fi
}

@test "version and OpenAPI describe the running build" {
  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/version"
  assert_success
  run jq -er '.version == "e2e"' <<<"${output}"
  assert_success

  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/api/openapi.json"
  assert_success
  run jq -er '.openapi | startswith("3.")' <<<"${output}"
  assert_success

  run curl --fail --silent --show-error "${SUXEN_E2E_URL}/api/v1"
  assert_success
  run jq -er '
    .versions[0].path == "/api/v1" and
    .versions[0].openapi == "/api/openapi.json" and
    .mounts.defaultOCI == "/v2/"
  ' <<<"${output}"
  assert_success
}

@test "the OCI discovery endpoint negotiates Basic authentication" {
  run curl \
    --silent \
    --show-error \
    --dump-header - \
    --output /dev/null \
  "${SUXEN_E2E_URL}/v2/"
  assert_success
  local headers="${output,,}"
  assert_contains "${headers}" "401"
  assert_contains "${headers}" 'www-authenticate: basic realm="suxen"'
  assert_contains "${headers}" "docker-distribution-api-version: registry/2.0"
}
