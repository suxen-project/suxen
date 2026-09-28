#!/usr/bin/env bats

load test_helper
load lib/cosign

setup() {
  setup_e2e
}

oci_reader_status() {
  local path="$1"
  curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --user trust-reader:trust-reader-password \
    "${SUXEN_E2E_URL}${path}"
}

@test "a policy change invalidates a signed Raw asset by fingerprint" {
  require_full_interop
  require_command cosign
  require_command openssl

  generate_cosign_key_pair "${E2E_CASE_DIRECTORY}"
  install_cosign_trust_policy raw "${E2E_CASE_DIRECTORY}/cosign.pub" verify-on-push >/dev/null
  printf 'signed raw payload\n' >"${E2E_CASE_DIRECTORY}/artifact.txt"

  local digest="sha256:$(sha256sum "${E2E_CASE_DIRECTORY}/artifact.txt" | cut -d ' ' -f 1)"
  printf '%s' "${digest}" >"${E2E_CASE_DIRECTORY}/digest.txt"
  COSIGN_PASSWORD="" cosign sign-blob \
    --key "${E2E_CASE_DIRECTORY}/cosign.key" \
    --output-signature "${E2E_CASE_DIRECTORY}/signature.txt" \
    --tlog-upload=false \
    --yes \
    "${E2E_CASE_DIRECTORY}/digest.txt" >/dev/null

  local signature
  signature="$(tr -d '\r\n' <"${E2E_CASE_DIRECTORY}/signature.txt")"
  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --header "Authorization: Bearer ${SUXEN_E2E_ADMIN_TOKEN}" \
    --header "X-Suxen-Signature: ${signature}" \
    --upload-file "${E2E_CASE_DIRECTORY}/artifact.txt" \
    "${SUXEN_E2E_URL}/repository/raw/provenance/signed.txt"
  assert_success

  local asset
  asset="$(asset_id raw provenance/signed.txt)"
  local fingerprint
  fingerprint="$(openssl pkey \
    -pubin \
    -in "${E2E_CASE_DIRECTORY}/cosign.pub" \
    -outform DER 2>/dev/null |
    sha256sum |
    cut -d ' ' -f 1)"
  local denied_policy
  denied_policy="$(jq -n \
    --arg fingerprint "${fingerprint}" \
    --rawfile key "${E2E_CASE_DIRECTORY}/cosign.pub" \
    '{
      mode:"verify-on-pull",
      publicKeys:[$key],
      certificateAuthorities:[],
      allowedIdentities:[],
      deniedFingerprints:[$fingerprint]
    }')"
  api PUT /api/v1/repositories/raw/trust-policy "${denied_policy}" >/dev/null

  run api POST "/api/v1/repositories/raw/assets/${asset}/verification" \
    "$(jq -n --arg signature "${signature}" '{signature:$signature}')"
  assert_failure
  assert_contains "${output}" 'denylist'
  run api_status GET /repository/raw/provenance/signed.txt
  assert_success
  assert_equal "${output}" "403"
  api DELETE /api/v1/repositories/raw/trust-policy >/dev/null
}

@test "cosign OCI 1.1 referrer verification releases a signed subject" {
  require_full_interop
  require_command cosign
  require_command crane

  generate_cosign_key_pair "${E2E_CASE_DIRECTORY}"
  local image="interop/cosign-referrer"
  local reference="${SUXEN_E2E_WRITE_REGISTRY}/${image}"
  local layer="${E2E_CASE_DIRECTORY}/subject-layer.tar"
  mkdir -p "${E2E_CASE_DIRECTORY}/subject"
  printf 'cosign OCI 1.1 subject\n' >"${E2E_CASE_DIRECTORY}/subject/payload.txt"
  tar -C "${E2E_CASE_DIRECTORY}/subject" -cf "${layer}" payload.txt

  crane auth login \
    --insecure \
    --username admin \
    --password admin-password \
    "${SUXEN_E2E_WRITE_REGISTRY}" >/dev/null
  crane append \
    --insecure \
    --oci-empty-base \
    --new_layer "${layer}" \
    --new_tag "${reference}:subject" >/dev/null
  local digest
  digest="$(crane digest --insecure "${reference}:subject")"

  install_cosign_trust_policy \
    oci \
    "${E2E_CASE_DIRECTORY}/cosign.pub" \
    verify-on-pull >/dev/null
  api POST /api/v1/roles '{
    "name":"trust-reader",
    "description":"Read-only subject for trust-policy verification",
    "privileges":["repository:oci:read"]
  }' >/dev/null
  api POST /api/v1/users '{
    "username":"trust-reader",
    "password":"trust-reader-password",
    "admin":false,
    "roles":["trust-reader"]
  }' >/dev/null
  # Ordinary readers remain quarantined. An authenticated publisher gets the
  # narrow digest-manifest bootstrap read that Cosign needs before attaching
  # its OCI 1.1 signature.
  run oci_reader_status "/v2/${image}/manifests/${digest}"
  assert_success
  assert_equal "${output}" "403"

  run env COSIGN_PASSWORD= COSIGN_EXPERIMENTAL=1 cosign sign \
    --allow-http-registry \
    --registry-referrers-mode=oci-1-1 \
    --registry-username admin \
    --registry-password admin-password \
    --key "${E2E_CASE_DIRECTORY}/cosign.key" \
    --tlog-upload=false \
    --yes \
    "${reference}@${digest}"
  assert_success

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/v2/${image}/referrers/${digest}"
  assert_success
  local referrers_payload="${output}"
  run jq -e '
    .schemaVersion == 2 and
    (.manifests | length == 1) and
    (.manifests[0].mediaType == "application/vnd.oci.image.manifest.v1+json") and
    (.manifests[0].digest | startswith("sha256:"))
  ' <<<"${referrers_payload}"
  if [[ "${status}" -ne 0 ]]; then
    printf 'OCI referrers response:\n%s\n' "${referrers_payload}" >&2
  fi
  assert_success

  # The following subject read runs Suxen's verify-on-pull path against the
  # signature that the real Cosign client just attached through OCI referrers.
  run oci_reader_status "/v2/${image}/manifests/${digest}"
  assert_success
  assert_equal "${output}" "200"

  api DELETE /api/v1/repositories/oci/trust-policy >/dev/null
}
