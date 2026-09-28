#!/usr/bin/env bats
# Boots the example (classification bound to the raw repo), uploads assets under
# two path prefixes, and checks each got the right classification label.
# Hermetic: suxenctl (admin) + curl (upload) only.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

@test "assets are labelled by classification rules as they are stored" {
	require_docker
	printf 'v1\n' >"$SUXEN_EXAMPLE_DATA/app.bin"

	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/app.bin "$SUXEN_URL/repository/raw/releases/app.bin"
	[ "$status" -eq 0 ]
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/app.bin "$SUXEN_URL/repository/raw/snapshots/app.bin"
	[ "$status" -eq 0 ]

	# The classification.tier attribute reflects the matching rule.
	run suxenctl attribute get raw "$(asset_id raw releases/app.bin)" classification
	[ "$status" -eq 0 ]
	[[ "$output" == *release* ]]

	run suxenctl attribute get raw "$(asset_id raw snapshots/app.bin)" classification
	[ "$status" -eq 0 ]
	[[ "$output" == *snapshot* ]]
}
