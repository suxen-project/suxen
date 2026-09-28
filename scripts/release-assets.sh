#!/usr/bin/env bash

release_asset_names() {
  local version="$1"
  printf '%s\n' \
    SHA256SUMS \
    "suxen-${version}-darwin-amd64.tar.gz" \
    "suxen-${version}-darwin-arm64.tar.gz" \
    "suxen-${version}-linux-amd64.tar.gz" \
    "suxen-${version}-linux-arm64.tar.gz" \
    "suxen-${version}-windows-amd64.zip" \
    "suxen-${version}-windows-arm64.zip" \
    "suxen-chart-${version}.tgz"
}
