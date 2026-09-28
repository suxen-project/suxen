#!/usr/bin/env bats
# Boots the receiver stack + a suxen that delivers to it, uploads a raw asset,
# and waits for the signed asset.uploaded event to arrive at the receiver.

load "../lib/harness.bash"

CURL="curlimages/curl:8.11.1"
SINK="http://127.0.0.1:9080"

setup_file() {
	require_docker
	# Build the receiver image up front so the readiness window covers boot.
	docker compose -f "$BATS_TEST_DIRNAME/compose.yaml" build -q
	suxen_start "$BATS_TEST_DIRNAME"
}
teardown_file() { suxen_stop; }

@test "an upload delivers a signed asset.uploaded event to the receiver" {
	require_docker
	client "$CURL" -fsS -X DELETE "$SINK/events" >/dev/null

	printf 'webhook payload\n' >"$SUXEN_EXAMPLE_DATA/event.txt"
	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/event.txt "$SUXEN_URL/repository/raw/webhook/event.txt"
	[ "$status" -eq 0 ]

	# Delivery is asynchronous; poll the receiver's recorded events.
	local delivered=""
	for _ in $(seq 1 60); do
		if client "$CURL" -fsS "$SINK/events" | grep -q "asset.uploaded"; then
			delivered=yes; break
		fi
		sleep 0.5
	done
	[ -n "$delivered" ]
}
