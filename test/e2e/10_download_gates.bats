#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
  curl \
    --silent \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --request DELETE \
    "${SUXEN_E2E_URL}/api/v1/repositories/raw/trust-policy" >/dev/null
  curl \
    --silent \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --request DELETE \
    "${SUXEN_E2E_URL}/api/v1/repositories/oci/trust-policy" >/dev/null
}

gate_payload='{"criteria":[{"path":"scan.status","op":"=","value":"passed"}],"enabled":true}'

@test "OCI manifest quarantine gate supports release and re-quarantine" {
  require_full_interop

  api PUT /api/v1/repositories/oci/download-gate "${gate_payload}" >/dev/null

  run api_status GET /v2/interop/hosted/manifests/docker "${SUXEN_E2E_ADMIN_TOKEN}"
  assert_success
  assert_equal "${output}" "403"

  local asset
  asset="$(api GET '/api/v1/repositories/oci/assets?prefix=v2/interop/hosted/manifests/docker' |
    jq -er '.items[] | select(.reference == "docker") | .id')"
  local digest
  digest="$(api GET '/api/v1/repositories/oci/assets?prefix=v2/interop/hosted/manifests/docker' |
    jq -er '.items[] | select(.reference == "docker") | .digest')"
  api_if_match PUT "/api/v1/repositories/oci/assets/${asset}/attributes/scan" \
    "${digest}" '{"status":"passed"}' >/dev/null
  run api_status GET /v2/interop/hosted/manifests/docker "${SUXEN_E2E_ADMIN_TOKEN}"
  assert_success
  assert_equal "${output}" "200"

  api_if_match DELETE "/api/v1/repositories/oci/assets/${asset}/attributes/scan" \
    "${digest}" >/dev/null
  run api_status GET /v2/interop/hosted/manifests/docker "${SUXEN_E2E_ADMIN_TOKEN}"
  assert_success
  assert_equal "${output}" "403"
  api DELETE /api/v1/repositories/oci/download-gate >/dev/null
}
