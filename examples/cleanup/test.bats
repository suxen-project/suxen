#!/usr/bin/env bats
# Boots the example (cleanup policy on the raw repo), uploads three assets under
# cleanup/, runs the policy, and checks the two oldest were removed and the
# newest kept (keepLast: 1). Hermetic: suxenctl (admin) + curl (upload/download).

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

upload() {
	printf '%s\n' "$1" >"$SUXEN_EXAMPLE_DATA/f"
	client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/f "$SUXEN_URL/repository/raw/cleanup/$1.txt"
}
code() {
	client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/raw/cleanup/$1.txt"
}

@test "cleanup deletes matching assets beyond keepLast" {
	require_docker
	# Sequential uploads: three.txt is newest.
	upload one; upload two; upload three

	# A dry run previews; --apply performs the deletion.
	run suxenctl cleanup --apply raw raw-cleanup
	[ "$status" -eq 0 ]

	run code three; [ "$output" = "200" ]   # newest kept
	run code one;   [ "$output" = "404" ]   # oldest deleted
	run code two;   [ "$output" = "404" ]
}
