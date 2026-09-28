#!/usr/bin/env bats
# Boots the example (an instance-wide default download gate) and exercises how a
# repository composes with it: a repo with no gate inherits the default; a repo
# with its own gate AND-extends the default; a repo opts out with
# inheritGlobal=false. Hermetic: suxenctl (admin) + curl (upload/download).

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

# code REPO PATH -> the HTTP status of an (anonymous) download.
code() { client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/$1/$2"; }
# upload REPO PATH -> PUT a small payload as the admin.
upload() {
	printf 'payload\n' >"$SUXEN_EXAMPLE_DATA/body"
	client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/body "$SUXEN_URL/repository/$1/$2"
}

setup_file_attrs() {
	printf '{"status":"passed"}\n' >"$SUXEN_EXAMPLE_DATA/scan.json"
	printf '{"status":"approved"}\n' >"$SUXEN_EXAMPLE_DATA/review.json"
}

@test "a repository with no gate inherits the instance-wide default" {
	require_docker
	setup_file_attrs
	run upload strict a.bin; [ "$status" -eq 0 ]
	run code strict a.bin; [ "$output" = "403" ]      # default withholds: no scan

	suxenctl attribute set --if-match "$(asset_digest strict a.bin)" \
		strict "$(asset_id strict a.bin)" scan \
		"$SUXEN_EXAMPLE_DATA/scan.json" >/dev/null
	run code strict a.bin; [ "$output" = "200" ]      # default satisfied
}

@test "a repository's own gate AND-extends the default" {
	require_docker
	setup_file_attrs
	run upload reviewed b.bin; [ "$status" -eq 0 ]
	local id digest
	id="$(asset_id reviewed b.bin)"
	digest="$(asset_digest reviewed b.bin)"

	# Satisfy only the repository's own criterion; the inherited default remains.
	suxenctl attribute set --if-match "$digest" reviewed "$id" review \
		"$SUXEN_EXAMPLE_DATA/review.json" >/dev/null
	run code reviewed b.bin; [ "$output" = "403" ]    # default's scan still missing

	suxenctl attribute set --if-match "$digest" reviewed "$id" scan \
		"$SUXEN_EXAMPLE_DATA/scan.json" >/dev/null
	run code reviewed b.bin; [ "$output" = "200" ]    # scan AND review both pass
}

@test "a repository opts out of the default with inheritGlobal=false" {
	require_docker
	run upload open c.bin; [ "$status" -eq 0 ]
	# With no gate of its own, `open` inherits the default and withholds.
	run code open c.bin; [ "$output" = "403" ]

	# A runtime gate with inheritGlobal=false and empty criteria gates nothing.
	# (inheritGlobal is runtime-only; a provisioned gate always inherits.)
	printf '{"criteria":[],"enabled":true,"inheritGlobal":false}\n' \
		>"$SUXEN_EXAMPLE_DATA/optout.json"
	suxenctl download-gate set open "$SUXEN_EXAMPLE_DATA/optout.json" >/dev/null
	run code open c.bin; [ "$output" = "200" ]
}
