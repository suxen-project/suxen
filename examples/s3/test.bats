#!/usr/bin/env bats
# Boots the MinIO stack + a suxen backed by it, uploads a raw asset, checks the
# round-trip, and confirms the bytes actually landed in the S3 bucket.

load "../lib/harness.bash"
bats_require_minimum_version 1.5.0

CURL="curlimages/curl:8.11.1"

setup_file() {
	require_docker
	# Build before the harness's short readiness window starts.
	docker compose -f "$BATS_TEST_DIRNAME/compose.yaml" build minio mc
	suxen_start "$BATS_TEST_DIRNAME"
}
teardown_file() { suxen_stop; }

@test "a raw asset round-trips through suxen and is stored in S3" {
	require_docker
	printf 'stored in s3\n' >"$SUXEN_EXAMPLE_DATA/hello.txt"

	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/hello.txt "$SUXEN_URL/repository/files/demo/hello.txt"
	[ "$status" -eq 0 ]
	run client "$CURL" -fsS "$SUXEN_URL/repository/files/demo/hello.txt"
	[ "$status" -eq 0 ]
	[ "$output" = "stored in s3" ]

	# The blob store now holds at least one object under the bucket.
	run --separate-stderr docker compose -f "$BATS_TEST_DIRNAME/compose.yaml" \
		run --rm --no-deps mc \
		ls --recursive local/suxen-example
	[ "$status" -eq 0 ]
	[ -n "$output" ]
}
