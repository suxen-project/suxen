#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

seed_upstream_manifest() {
  local image="$1"
  local tag="$2"
  local layer="${E2E_CASE_DIRECTORY}/${tag}.tar"
  tar -C "${BATS_TEST_DIRNAME}/fixtures/image" -cf "${layer}" payload.txt
  crane append \
    --insecure \
    --new_layer "${layer}" \
    --new_tag "${SUXEN_E2E_UPSTREAM_REGISTRY}/${image}:${tag}" >/dev/null
}

upstream_registry_request_count() {
  local request_uri="$1"
  compose logs --no-color upstream-registry 2>&1 |
    grep --fixed-strings --count "\"path\":\"${request_uri}\"" || true
}

upstream_request_count_exceeds() {
  local request_uri="$1"
  local baseline="$2"
  local current
  current="$(upstream_registry_request_count "${request_uri}")"
  ((current > baseline))
}

registry_token_request_count() {
  compose exec -T registry-auth \
    wget -qO- http://127.0.0.1:8080/requests |
    jq -er '.requests'
}

@test "OCI proxy pulls through a conformant upstream and serves the second request from cache" {
  require_full_interop
  require_command crane

  seed_upstream_manifest interop/proxy cache-me
  local request_uri="/v2/interop/proxy/manifests/cache-me"
  local before
  before="$(upstream_registry_request_count "${request_uri}")"

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    --output "${E2E_CASE_DIRECTORY}/first-manifest.json" \
    "${SUXEN_E2E_URL}/repository/oci-proxy/v2/interop/proxy/manifests/cache-me"
  assert_success
  wait_until "upstream registry manifest request log" 10 \
    upstream_request_count_exceeds "${request_uri}" "${before}"
  local after_first
  after_first="$(upstream_registry_request_count "${request_uri}")"

  run curl \
    --fail-with-body \
    --silent \
    --show-error \
    --user admin:admin-password \
    --output "${E2E_CASE_DIRECTORY}/second-manifest.json" \
    "${SUXEN_E2E_URL}/repository/oci-proxy/v2/interop/proxy/manifests/cache-me"
  assert_success
  run cmp \
    "${E2E_CASE_DIRECTORY}/first-manifest.json" \
    "${E2E_CASE_DIRECTORY}/second-manifest.json"
  assert_success
  # Allow the registry's structured log stream to flush. The cache TTL is 30s,
  # so this does not risk turning the cache assertion into a revalidation.
  sleep 2
  assert_equal "$(upstream_registry_request_count "${request_uri}")" "${after_first}"
}

@test "OCI proxy caches upstream misses" {
  local request_uri="/v2/interop/missing/manifests/not-here"
  local path="/repository/oci-proxy${request_uri}"
  local before
  before="$(upstream_registry_request_count "${request_uri}")"

  run api_status GET "${path}"
  assert_success
  assert_equal "${output}" "404"
  wait_until "upstream registry missing-manifest request log" 10 \
    upstream_request_count_exceeds "${request_uri}" "${before}"
  local after_first
  after_first="$(upstream_registry_request_count "${request_uri}")"

  run api_status GET "${path}"
  assert_success
  assert_equal "${output}" "404"

  sleep 2
  assert_equal "$(upstream_registry_request_count "${request_uri}")" "${after_first}"
}

@test "proxy rejects writes" {
  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --request PUT \
    --user admin:admin-password \
    --header 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    --data '{}' \
    "${SUXEN_E2E_URL}/repository/oci-proxy/v2/interop/proxy/manifests/rejected"
  assert_success
  assert_equal "${output}" "405"
}

@test "proxy authenticates to an upstream Bearer token service" {
  require_full_interop

  local before
  before="$(registry_token_request_count)"

  run api_status GET \
    /repository/oci-auth-proxy/v2/interop/authenticated/manifests/missing
  assert_success
  assert_equal "${output}" "404"

  local after
  after="$(registry_token_request_count)"
  if ((after <= before)); then
    fail "the proxy did not request a signed Bearer token from the upstream issuer"
  fi
}
