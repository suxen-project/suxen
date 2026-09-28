#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "docker pushes to replica A and pulls through the load balancer" {
  require_full_interop
  require_command docker

  local write_image="${SUXEN_E2E_DOCKER_WRITE_REGISTRY}/interop/hosted:docker"
  local read_image="${SUXEN_E2E_DOCKER_REGISTRY}/interop/hosted:docker"
  registry_login "${SUXEN_E2E_DOCKER_WRITE_REGISTRY}"
  registry_login "${SUXEN_E2E_DOCKER_REGISTRY}"
  docker build --tag "${write_image}" "${BATS_TEST_DIRNAME}/fixtures/image"

  run docker push "${write_image}"
  assert_success

  docker image rm "${write_image}" >/dev/null
  run docker pull "${read_image}"
  assert_success
  run docker image inspect --format '{{.RepoTags}}' "${read_image}"
  assert_success
  assert_contains "${output}" "${read_image}"
}

@test "docker push upload sessions survive load-balancing across replicas" {
  require_full_interop

  local image="${SUXEN_E2E_DOCKER_REGISTRY}/interop/hosted:load-balanced"
  registry_login "${SUXEN_E2E_DOCKER_REGISTRY}"
  docker build --tag "${image}" "${BATS_TEST_DIRNAME}/fixtures/image"
  run docker push "${image}"
  assert_success
}

@test "manifest HEAD works by tag and immutable digest" {
  require_full_interop
  require_command crane

  local reference="${SUXEN_E2E_REGISTRY}/interop/hosted:docker"
  crane auth login \
    --insecure \
    --username admin \
    --password admin-password \
    "${SUXEN_E2E_REGISTRY}" >/dev/null
  local digest
  digest="$(crane digest --insecure "${reference}")"

  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --head \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/v2/interop/hosted/manifests/docker"
  assert_success
  assert_equal "${output}" "200"

  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --head \
    --user admin:admin-password \
    "${SUXEN_E2E_URL}/v2/interop/hosted/manifests/${digest}"
  assert_success
  assert_equal "${output}" "200"
}

@test "an oversized OCI layer is rejected with 413" {
  require_full_interop

  local layer="${E2E_CASE_DIRECTORY}/oversized-layer"
  dd if=/dev/zero of="${layer}" bs=1M count=17 status=none
  local digest="sha256:$(sha256sum "${layer}" | cut -d ' ' -f 1)"
  local upload_location
  upload_location="$(curl \
    --fail \
    --silent \
    --show-error \
    --dump-header - \
    --output /dev/null \
    --request POST \
    --user admin:admin-password \
    "http://${SUXEN_E2E_WRITE_REGISTRY}/v2/interop/limits/blobs/uploads/" |
    awk 'tolower($1) == "location:" {sub("\r$", "", $2); print $2}')"
  if [[ "${upload_location}" == /* ]]; then
    upload_location="http://${SUXEN_E2E_WRITE_REGISTRY}${upload_location}"
  fi

  run curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --request PUT \
    --user admin:admin-password \
    --upload-file "${layer}" \
    "${upload_location}?digest=${digest}"
  assert_success
  assert_equal "${output}" "413"
}

@test "a multi-platform index selects the requested platform" {
  require_full_interop
  require_command crane

  local repository="${SUXEN_E2E_WRITE_REGISTRY}/interop/platform"
  local amd64_layer="${E2E_CASE_DIRECTORY}/amd64-layer.tar"
  local arm64_layer="${E2E_CASE_DIRECTORY}/arm64-layer.tar"
  mkdir -p "${E2E_CASE_DIRECTORY}/amd64" "${E2E_CASE_DIRECTORY}/arm64"
  printf 'linux/amd64\n' >"${E2E_CASE_DIRECTORY}/amd64/platform.txt"
  printf 'linux/arm64\n' >"${E2E_CASE_DIRECTORY}/arm64/platform.txt"
  tar -C "${E2E_CASE_DIRECTORY}/amd64" -cf "${amd64_layer}" platform.txt
  tar -C "${E2E_CASE_DIRECTORY}/arm64" -cf "${arm64_layer}" platform.txt

  crane auth login \
    --insecure \
    --username admin \
    --password admin-password \
    "${SUXEN_E2E_WRITE_REGISTRY}" >/dev/null
  crane append \
    --insecure \
    --oci-empty-base \
    --new_layer "${amd64_layer}" \
    --new_tag "${repository}:amd64" >/dev/null
  crane mutate \
    --insecure \
    --set-platform linux/amd64 \
    --tag "${repository}:amd64-platform" \
    "${repository}:amd64" >/dev/null
  crane append \
    --insecure \
    --oci-empty-base \
    --new_layer "${arm64_layer}" \
    --new_tag "${repository}:arm64" >/dev/null
  crane mutate \
    --insecure \
    --set-platform linux/arm64 \
    --tag "${repository}:arm64-platform" \
    "${repository}:arm64" >/dev/null
  crane index append \
    --insecure \
    --manifest "${repository}:amd64-platform" \
    --manifest "${repository}:arm64-platform" \
    --tag "${repository}:multi" >/dev/null

  run crane config \
    --insecure \
    --platform linux/amd64 \
    "${SUXEN_E2E_REGISTRY}/interop/platform:multi"
  assert_success
  run jq -e '.os == "linux" and .architecture == "amd64"' <<<"${output}"
  assert_success

  run crane config \
    --insecure \
    --platform linux/arm64 \
    "${SUXEN_E2E_REGISTRY}/interop/platform:multi"
  assert_success
  run jq -e '.os == "linux" and .architecture == "arm64"' <<<"${output}"
  assert_success
}
