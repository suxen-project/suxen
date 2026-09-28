#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: smoke-runtime-image.sh IMAGE PLATFORM EXPECTED_VERSION}"
platform="${2:?usage: smoke-runtime-image.sh IMAGE PLATFORM EXPECTED_VERSION}"
expected_version="${3:?usage: smoke-runtime-image.sh IMAGE PLATFORM EXPECTED_VERSION}"
platform_label="${platform//\//-}"
container="suxen-runtime-smoke-${platform_label}-${RANDOM}-$$"

cleanup() {
  docker rm --force "${container}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run --platform "${platform}" --detach --rm --name "${container}" \
  --env SUXEN_BOOTSTRAP_USER=admin \
  --env SUXEN_BOOTSTRAP_PASSWORD=runtime-smoke-password \
  --env SUXEN_BOOTSTRAP_TOKEN=runtime-smoke-token-at-least-24-characters \
  "${image}" >/dev/null

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
printf 'runtime image %s (%s) did not become ready\n' "${image}" "${platform}" >&2
exit 1
