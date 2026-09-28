#!/usr/bin/env bats
# Boots the example (download gate on the raw repo), uploads an asset, and
# checks it is withheld (403) until scan.status=passed, then withheld again once
# the attribute is cleared. Hermetic: suxenctl (admin) + curl (upload/download).

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

code() {
	client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/raw/gated/app.bin"
}

@test "a gated asset is released only while it passes the gate criteria" {
	require_docker
	printf 'payload\n' >"$SUXEN_EXAMPLE_DATA/app.bin"
	printf '{"status":"passed"}\n' >"$SUXEN_EXAMPLE_DATA/scan.json"

	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/app.bin "$SUXEN_URL/repository/raw/gated/app.bin"
	[ "$status" -eq 0 ]

	local id digest
	id="$(asset_id raw gated/app.bin)"
	digest="$(asset_digest raw gated/app.bin)"

	run code; [ "$output" = "403" ]                       # withheld: no scan yet

	suxenctl attribute set --if-match "$digest" raw "$id" scan \
		"$SUXEN_EXAMPLE_DATA/scan.json" >/dev/null
	run code; [ "$output" = "200" ]                       # released: scan passed

	suxenctl attribute delete --if-match "$digest" raw "$id" scan >/dev/null
	run code; [ "$output" = "403" ]                       # re-quarantined
}
