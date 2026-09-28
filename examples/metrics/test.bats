#!/usr/bin/env bats
# Boots the example and scrapes GET /metrics: it requires an admin-scoped token
# (401 anonymous, 403 for a non-admin token, 200 for admin) and exposes suxen's
# Prometheus instrumentation. Hermetic: suxenctl (admin) + curl.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

@test "the metrics endpoint requires a token with admin:stats:read" {
	require_docker

	# Anonymous scrape is unauthenticated.
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/metrics"
	[ "$output" = "401" ]

	# A valid but non-admin token lacks admin:stats:read.
	suxenctl user create --password scraper-pass-123 --roles reader scraper >/dev/null
	local reader_token
	reader_token="$(suxenctl user token --name t --scopes repository:*:read scraper \
		| grep -o '"token":"[^"]*"' | cut -d'"' -f4)"
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $reader_token" "$SUXEN_URL/metrics"
	[ "$output" = "403" ]

	# The bootstrap admin token has it.
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $SUXEN_TOKEN" "$SUXEN_URL/metrics"
	[ "$output" = "200" ]
}

@test "the endpoint exposes suxen instrumentation series" {
	require_docker

	# Generate some traffic and blob activity to report on.
	printf 'payload\n' >"$SUXEN_EXAMPLE_DATA/body"
	client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/body "$SUXEN_URL/repository/files/m.txt" >/dev/null

	run client "$CURL" -fsS -H "Authorization: Bearer $SUXEN_TOKEN" "$SUXEN_URL/metrics"
	[ "$status" -eq 0 ]
	echo "$output" | grep -q '^suxen_http_requests_total'
	echo "$output" | grep -q '^suxen_blob_operations_total'
	echo "$output" | grep -q '^suxen_repositories'
	echo "$output" | grep -q '^suxen_uptime_seconds'
}
