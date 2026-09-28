#!/usr/bin/env bash
# Start the MinIO support stack, then a local suxen backed by that S3 store.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose=(docker compose -f "$here/compose.yaml")
bucket="suxen-example"

"${compose[@]}" build minio mc
"${compose[@]}" up -d --wait minio

launcher_pid=""
cleanup() {
	[ -n "$launcher_pid" ] && kill "$launcher_pid" 2>/dev/null || true
	"${compose[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# Create the bucket (idempotent) now that MinIO is healthy.
"${compose[@]}" run --rm --no-deps mc mb --ignore-existing "local/$bucket" >/dev/null

# Point suxen's blob store at MinIO. The launcher honours an existing
# SUXEN_BLOBSTORE and inherits the AWS credentials from this environment.
export SUXEN_BLOBSTORE="s3://$bucket/blobs?region=us-east-1&endpoint=http://127.0.0.1:9000&pathStyle=true"
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin

"$here/../suxen-with-mem-blobstore" -f "$here/repo.yaml" "$@" &
launcher_pid=$!
wait "$launcher_pid"
