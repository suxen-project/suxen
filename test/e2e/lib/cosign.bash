# shellcheck shell=bash

generate_cosign_key_pair() {
  local directory="$1"
  mkdir -p "${directory}"
  COSIGN_PASSWORD="" cosign generate-key-pair --output-key-prefix "${directory}/cosign" >/dev/null
}

install_cosign_trust_policy() {
  local repository="$1"
  local public_key_file="$2"
  local mode="$3"
  local payload
  payload="$(jq -n \
    --arg mode "${mode}" \
    --rawfile key "${public_key_file}" \
    '{mode: $mode, publicKeys: [$key], certificateAuthorities: [], allowedIdentities: [], deniedFingerprints: []}')"
  api PUT "/api/v1/repositories/${repository}/trust-policy" "${payload}"
}

cosign_sign_image() {
  local image="$1"
  local private_key_file="$2"
  COSIGN_PASSWORD="" cosign sign \
    --allow-insecure-registry \
    --key "${private_key_file}" \
    --tlog-upload=false \
    --yes \
    "${image}"
}
