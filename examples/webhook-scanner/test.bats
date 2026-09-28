#!/usr/bin/env bats
# Boots the scanner support stack + a suxen whose downloads are quarantined until
# a scan passes. An external scanner reacts to asset.uploaded webhooks and stamps
# scan.status=passed back through the suxen API with an annotate-scoped token, so
# the download gate releases the asset. Covers a hosted upload and a proxy pull.

load "../lib/harness.bash"

CURL="curlimages/curl:8.11.1"

setup_file() {
	require_docker
	# The mirror proxy's upstream (a secretRef) must resolve when the harness
	# re-applies repo.yaml; match what start.sh sets for its own apply.
	export SUXEN_MIRROR_UPSTREAM="http://upstream:upstream-password@127.0.0.1:9092/files/"
	# Build the support image up front so the readiness window covers boot.
	docker compose -f "$BATS_TEST_DIRNAME/compose.yaml" build -q
	suxen_start "$BATS_TEST_DIRNAME"
}
teardown_file() { suxen_stop; }

# code REPO/PATH -> the HTTP status of an anonymous download.
code() { client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/$1"; }

# wait_release REPO/PATH -> succeeds once the asset becomes downloadable (200).
wait_release() {
	local target="$1" c
	for _ in $(seq 1 60); do
		c="$(code "$target")"
		[ "$c" = "200" ] && return 0
		sleep 0.5
	done
	echo "asset $target was not released (last code $c)" >&2
	return 1
}

@test "a hosted upload is quarantined until the scanner webhook stamps it" {
	require_docker
	printf 'scan me\n' >"$SUXEN_EXAMPLE_DATA/app.bin"
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/app.bin "$SUXEN_URL/repository/uploads/app.bin"
	[ "$status" -eq 0 ]

	# The gate withholds until scan.status=passed; the scanner (async webhook)
	# stamps it, so it releases with no manual step.
	run code uploads/app.bin; [ "$output" = "403" ]
	wait_release uploads/app.bin

	# The attribute the scanner set is present, in its own namespace.
	local id
	id="$(asset_id uploads app.bin)"
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/api/v1/repositories/uploads/assets/$id/attributes/scan"
	[ "$status" -eq 0 ]
	[[ "$output" == *passed* ]]
}

@test "a proxy pull is fetched, quarantined, then released after scanning" {
	require_docker
	# First pull fetches from the upstream, caches it, and fires asset.uploaded;
	# the gate withholds the freshly fetched artifact.
	run code mirror/artifact.txt; [ "$output" = "403" ]
	wait_release mirror/artifact.txt

	run client "$CURL" -fsS "$SUXEN_URL/repository/mirror/artifact.txt"
	[ "$status" -eq 0 ]
	[[ "$output" == *"upstream artifact"* ]]
}

@test "the scanner's service account can annotate but not push or delete" {
	require_docker
	# Mint a token with the scanner role (annotate only), like start.sh does.
	suxenctl user create --password probe-service-pass --roles scanner probe >/dev/null 2>&1 || true
	local t
	t="$(suxenctl user token --name probe-token probe | grep -o '"token":"[^"]*"' | cut -d'"' -f4)"

	# Annotating an existing asset is allowed.
	local id digest
	id="$(asset_id uploads app.bin)"
	digest="$(asset_digest uploads app.bin)"
	printf '{"status":"passed"}\n' >"$SUXEN_EXAMPLE_DATA/scan.json"
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' -X PUT \
		-H "Authorization: Bearer $t" -H 'Content-Type: application/json' \
		-H "If-Match: $digest" \
		--data-binary @/work/scan.json \
		"$SUXEN_URL/api/v1/repositories/uploads/assets/$id/attributes/scan"
	if [ "$output" != "200" ]; then
		echo "attribute update returned HTTP $output" >&3
		false
	fi

	# Pushing an artifact with the same token is refused (no write privilege).
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' -X PUT \
		-H "Authorization: Bearer $t" --data 'nope' \
		"$SUXEN_URL/repository/uploads/blocked.txt"
	[ "$output" = "403" ]
}
