#!/usr/bin/env bats
# Boots the example, then exercises the client commands from README.md.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

@test "authenticated upload then anonymous download preserves bytes" {
	require_docker
	dd if=/dev/urandom of="$SUXEN_EXAMPLE_DATA/source.bin" bs=1024 count=32 status=none

	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/source.bin "$SUXEN_URL/repository/files/demo/source.bin"
	[ "$status" -eq 0 ]

	run client "$CURL" -fsS --output /work/downloaded.bin \
		"$SUXEN_URL/repository/files/demo/source.bin"
	[ "$status" -eq 0 ]
	cmp "$SUXEN_EXAMPLE_DATA/source.bin" "$SUXEN_EXAMPLE_DATA/downloaded.bin"
}

@test "anonymous upload is refused" {
	require_docker
	printf 'nope\n' >"$SUXEN_EXAMPLE_DATA/nope.txt"

	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		--upload-file /work/nope.txt "$SUXEN_URL/repository/files/demo/nope.txt"
	[ "$status" -eq 0 ]
	[ "$output" = "401" ] || [ "$output" = "403" ]
}

@test "replacement policy allows changed files or protects existing paths with idempotent retries" {
	require_docker
	printf 'original\n' >"$SUXEN_EXAMPLE_DATA/original.txt"
	printf 'replacement\n' >"$SUXEN_EXAMPLE_DATA/replacement.txt"
	for repo in files releases; do
		for attempt in 1 2; do
			run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
				-H "Authorization: Bearer $SUXEN_TOKEN" --upload-file /work/original.txt \
				"$SUXEN_URL/repository/$repo/policy/file.txt"
			[ "$status" -eq 0 ]
			[ "$output" = "201" ]
		done
		run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
			-H "Authorization: Bearer $SUXEN_TOKEN" --upload-file /work/replacement.txt \
			"$SUXEN_URL/repository/$repo/policy/file.txt"
		[ "$status" -eq 0 ]
		if [ "$repo" = "files" ]; then [ "$output" = "201" ]; else [ "$output" = "409" ]; fi
		run client "$CURL" -fsS "$SUXEN_URL/repository/$repo/policy/file.txt"
		[ "$status" -eq 0 ]
		if [ "$repo" = "files" ]; then [ "$output" = "replacement" ]; else [ "$output" = "original" ]; fi
	done
}

@test "component listing groups a version directory with its side files" {
	require_docker
	printf 'model\n' >"$SUXEN_EXAMPLE_DATA/model.txt"
	# 0.9.0 is uploaded last, so only version order keeps 0.10.0 in the next test.
	for version in 0.10.0 0.9.0; do
		for file in core.glb SHA256SUMS; do
			run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
				--upload-file /work/model.txt "$SUXEN_URL/repository/models/models/core/$version/$file"
			[ "$status" -eq 0 ]
		done
	done
	# Without a .glb anchor, a checksum-only directory is not a version.
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/model.txt "$SUXEN_URL/repository/models/models/core/0.11.0/SHA256SUMS"
	[ "$status" -eq 0 ]

	run suxenctl repo components models
	[ "$status" -eq 0 ]
	[ "$(grep -o '"version":"[^"]*"' <<<"$output" | tr '\n' ' ')" = '"version":"0.10.0" "version":"0.9.0" ' ]
	grep -q '"path":"models/core/0.10.0/SHA256SUMS"' <<<"$output"
}

@test "version-ordered cleanup deletes whole lower versions" {
	require_docker
	run suxenctl cleanup --apply models keep-latest-model
	[ "$status" -eq 0 ]
	for file in core.glb SHA256SUMS; do
		run client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/models/models/core/0.9.0/$file"
		[ "$output" = "404" ]
		run client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/models/models/core/0.10.0/$file"
		[ "$output" = "200" ]
	done
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/models/models/core/0.11.0/SHA256SUMS"
	[ "$output" = "200" ]
}

@test "a component pattern without a version group is rejected" {
	run suxenctl repo create --component '^(?P<name>.+)$' bad-components
	[ "$status" -ne 0 ]
	[[ "$output" == *invalid_format_config* ]]
}
