#!/usr/bin/env bats
# Boots the example (a second, fs-backed blob store `archive`), and exercises
# per-repository store binding, GC of unreferenced blobs, and draining a store.
# Hermetic: suxenctl + curl, no network.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

usage() { # STORE FIELD -> integer
	client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/api/v1/blob-stores/$1/usage" \
		| grep -o "\"$2\":[0-9]*" | grep -o '[0-9]*'
}
upload() {
	printf '%s\n' "$2" >"$SUXEN_EXAMPLE_DATA/f"
	client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/f "$SUXEN_URL/repository/cold/$1"
}

@test "an asset lands in the repository's bound blob store, not the default" {
	require_docker
	run upload a.txt hello; [ "$status" -eq 0 ]
	[ "$(usage archive objectCount)" -ge 1 ]
	[ "$(usage default objectCount)" -eq 0 ]
}

@test "gc reclaims unreferenced blobs" {
	require_docker
	upload gc.txt disposable
	local before; before="$(usage archive objectCount)"
	# Deleting the asset leaves its blob unreferenced.
	client "$CURL" -fsS -X DELETE -H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/repository/cold/gc.txt" >/dev/null
	[ "$(usage archive unreferencedCount)" -ge 1 ]

	# grace 0 collects it immediately.
	run suxenctl gc --apply --grace 0s
	[ "$status" -eq 0 ]
	[ "$(usage archive unreferencedCount)" -eq 0 ]
}

@test "draining a blob store migrates its objects to the target" {
	require_docker
	# archive still holds the referenced a.txt from the first test.
	[ "$(usage archive objectCount)" -ge 1 ]
	client "$CURL" -fsS -X POST -H "Authorization: Bearer $SUXEN_TOKEN" \
		-H 'Content-Type: application/json' --data '{"target":"default"}' \
		"$SUXEN_URL/api/v1/blob-stores/archive/drain" >/dev/null

	# The migrate job (1s interval) copies objects to default and marks archive
	# drained.
	local drained=""
	for _ in $(seq 1 30); do
		if client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
			"$SUXEN_URL/api/v1/blob-stores/archive" | grep -q '"state":"drained"'; then
			drained=yes; break
		fi
		sleep 1
	done
	[ -n "$drained" ]
	[ "$(usage default objectCount)" -ge 1 ]
}
