#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: smoke-release-images.sh VERSION [PLATFORM ...]}"
shift
platforms=("$@")
if ((${#platforms[@]} == 0)); then
  case "$(uname -m)" in
    x86_64) platforms=(linux/amd64) ;;
    aarch64 | arm64) platforms=(linux/arm64) ;;
    *) printf 'unsupported native architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
  esac
fi

images=()
cleanup() {
  if ((${#images[@]} > 0)); then
    docker image rm --force "${images[@]}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

for platform in "${platforms[@]}"; do
  platform_label="${platform//\//-}"
  runtime_image="suxen-runtime-smoke:${version}-${platform_label}"
  sdk_image="suxen-sdk-smoke-base:${version}-${platform_label}"
  images+=("${runtime_image}" "${sdk_image}")

  # The SDK-derived image uses the just-loaded SDK image as its FROM source.
  # Keep these local smoke builds on Docker's daemon-backed builder so that
  # both the runtime image and that SDK base remain visible to later builds.
  docker buildx build --builder default --load --platform "${platform}" \
    --build-arg "VERSION=${version}" \
    --tag "${runtime_image}" .
  scripts/smoke-runtime-image.sh "${runtime_image}" "${platform}" "${version}"

  docker buildx build --builder default --load --platform "${platform}" \
    --file sdk/Dockerfile \
    --build-arg "VERSION=${version}" \
    --tag "${sdk_image}" .
  SUXEN_BUILDX_BUILDER=default \
    SUXEN_SDK_SMOKE_TAG="${version}-${platform_label}" \
    scripts/smoke-sdk-image.sh "${sdk_image}" "${platform}" "${version}"
done
