#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "OCI group returns the union of hosted and proxy tags" {
  require_full_interop
  require_command crane

  local layer="${E2E_CASE_DIRECTORY}/group-layer.tar"
  tar -C "${BATS_TEST_DIRNAME}/fixtures/image" -cf "${layer}" payload.txt
  crane append \
    --insecure \
    --new_layer "${layer}" \
    --new_tag "${SUXEN_E2E_UPSTREAM_REGISTRY}/interop/group:upstream" >/dev/null

  crane auth login \
    --insecure \
    --username admin \
    --password admin-password \
    "${SUXEN_E2E_WRITE_REGISTRY}" >/dev/null
  crane append \
    --insecure \
    --new_layer "${layer}" \
    --new_tag "${SUXEN_E2E_WRITE_REGISTRY}/interop/group:hosted" >/dev/null

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/repository/oci-group/v2/interop/group/tags/list"
  assert_success
  run jq -e '.tags | index("hosted") and index("upstream")' <<<"${output}"
  assert_success
}

@test "OCI group resolves a proxy member and remains read-only" {
  require_full_interop
  require_command crane

  local layer="${E2E_CASE_DIRECTORY}/fallback-layer.tar"
  tar -C "${BATS_TEST_DIRNAME}/fixtures/image" -cf "${layer}" payload.txt
  crane append \
    --insecure \
    --new_layer "${layer}" \
    --new_tag "${SUXEN_E2E_UPSTREAM_REGISTRY}/interop/group:fallback" >/dev/null

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/repository/oci-group/v2/interop/group/manifests/fallback"
  assert_success

  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --request PUT \
    --user admin:admin-password \
    --header 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    --data '{}' \
    "${SUXEN_E2E_URL}/repository/oci-group/v2/interop/group/manifests/rejected"
  assert_success
  assert_equal "${output}" "405"
}

@test "OCI group returns the union of referrers" {
  require_full_interop
  require_command oras
  require_command crane

  local image="interop/group-referrers"
  local hosted_reference="${SUXEN_E2E_WRITE_REGISTRY}/${image}"
  local upstream_reference="${SUXEN_E2E_UPSTREAM_REGISTRY}/${image}"
  local subject_layer="${E2E_CASE_DIRECTORY}/subject-layer.tar"
  mkdir -p "${E2E_CASE_DIRECTORY}/subject"
  printf 'shared subject\n' >"${E2E_CASE_DIRECTORY}/subject/payload.txt"
  tar -C "${E2E_CASE_DIRECTORY}/subject" -cf "${subject_layer}" payload.txt

  crane auth login \
    --insecure \
    --username admin \
    --password admin-password \
    "${SUXEN_E2E_WRITE_REGISTRY}" >/dev/null
  crane append \
    --insecure \
    --oci-empty-base \
    --new_layer "${subject_layer}" \
    --new_tag "${hosted_reference}:subject" >/dev/null
  local subject_digest
  subject_digest="$(crane digest --insecure "${hosted_reference}:subject")"
  crane copy \
    --insecure \
    "${hosted_reference}@${subject_digest}" \
    "${upstream_reference}:subject" >/dev/null
  assert_equal \
    "$(crane digest --insecure "${upstream_reference}:subject")" \
    "${subject_digest}"

  printf 'hosted referrer\n' >"${E2E_CASE_DIRECTORY}/hosted.txt"
  printf 'upstream referrer\n' >"${E2E_CASE_DIRECTORY}/upstream.txt"
  run bash -c '
    cd "$1"
    exec oras attach \
      --plain-http \
      --username admin \
      --password admin-password \
      --distribution-spec v1.1-referrers-api \
      --artifact-type application/vnd.suxen.e2e.hosted \
      "$2@$3" \
      hosted.txt:text/plain
  ' _ "${E2E_CASE_DIRECTORY}" "${hosted_reference}" "${subject_digest}"
  assert_success
  run bash -c '
    cd "$1"
    exec oras attach \
      --plain-http \
      --distribution-spec v1.1-referrers-api \
      --artifact-type application/vnd.suxen.e2e.upstream \
      "$2@$3" \
      upstream.txt:text/plain
  ' _ "${E2E_CASE_DIRECTORY}" "${upstream_reference}" "${subject_digest}"
  assert_success

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/repository/oci-group/v2/${image}/referrers/${subject_digest}"
  assert_success
  run jq -e '
    .schemaVersion == 2 and
    (.manifests | length == 2) and
    ([.manifests[].digest] | unique | length == 2)
  ' <<<"${output}"
  assert_success
}
