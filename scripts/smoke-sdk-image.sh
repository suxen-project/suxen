#!/usr/bin/env bash
set -euo pipefail

sdk_image="${1:-suxen-sdk:test}"
platform="${2:-linux/amd64}"
expected_version="${3:-sdk-smoke}"
platform_label="${platform//\//-}"
smoke_image="suxen-sdk-smoke:${SUXEN_SDK_SMOKE_TAG:-test-${platform_label}}"
container="suxen-sdk-smoke-${platform_label}-${RANDOM}-$$"
builder_args=()
if [[ -n ${SUXEN_BUILDX_BUILDER:-} ]]; then
  builder_args=(--builder "${SUXEN_BUILDX_BUILDER}")
fi

cleanup() {
  docker rm --force "${container}" >/dev/null 2>&1 || true
  docker image rm --force "${smoke_image}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker buildx build "${builder_args[@]}" --load --platform "${platform}" \
  --build-arg "SDK_IMAGE=${sdk_image}" \
  --build-arg "CUSTOM_VERSION=${expected_version}" \
  --file sdk/Dockerfile.smoke \
  --tag "${smoke_image}" \
  .

docker run --platform "${platform}" --detach --rm --name "${container}" \
  --env SUXEN_BOOTSTRAP_USER=admin \
  --env SUXEN_BOOTSTRAP_PASSWORD=sdk-smoke-password \
  --env SUXEN_BOOTSTRAP_TOKEN=sdk-smoke-token-at-least-24-characters \
  "${smoke_image}" >/dev/null
for _ in $(seq 1 60); do
  if docker exec "${container}" wget -q -O /tmp/version http://127.0.0.1:8080/version && \
     docker exec "${container}" wget -q -O /dev/null http://127.0.0.1:8080/readyz; then
    docker exec "${container}" grep -Fq "\"version\":\"${expected_version}\"" /tmp/version
    exit 0
  fi
  if ! docker inspect --format '{{.State.Running}}' "${container}" 2>/dev/null | grep -q true; then
    docker logs "${container}" >&2 || true
    exit 1
  fi
  sleep 1
done

docker logs "${container}" >&2 || true
printf 'SDK smoke distribution built from %s (%s) did not become ready\n' \
  "${sdk_image}" "${platform}" >&2
exit 1
