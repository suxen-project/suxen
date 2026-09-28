#!/usr/bin/env bats
# Boots the example, creates a scoped user + token, and proves access control:
# a token can do only the intersection of its scopes and its user's privileges,
# and anonymous access is read-only. Hermetic: suxenctl + curl, no network.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

# code METHOD REPO PATH [TOKEN] — HTTP status of a write/read as TOKEN (or anon).
put_code() {
	local repo="$1" path="$2" token="${3:-}"
	local auth=(); [ -n "$token" ] && auth=(-H "Authorization: Bearer $token")
	printf 'x\n' >"$SUXEN_EXAMPLE_DATA/payload"
	client "$CURL" -sS -o /dev/null -w '%{http_code}' "${auth[@]}" \
		--upload-file /work/payload "$SUXEN_URL/repository/$repo/$path"
}
get_code() {
	client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/$1/$2"
}

@test "a scoped token grants only the intersection of its scopes and the user's roles" {
	# The user holds the `writer` role (app + logs write); the token is scoped to
	# app only, so its effective power is repository:app:write.
	suxenctl user create --password ci-password-123 --roles writer ci >/dev/null
	local token
	token="$(suxenctl user token --name ci-token --scopes repository:app:write ci \
		| grep -o '"token":"[^"]*"' | cut -d'"' -f4)"
	[ -n "$token" ]

	# In scope: write to app succeeds.
	run put_code app demo/a.txt "$token"; [ "$output" = "201" ]

	# Out of scope: the user COULD write logs, but the token may not (scopes are
	# an intersection, not an extra grant).
	run put_code logs demo/a.txt "$token"; [ "$output" = "403" ]

	# whoami with the token reflects the intersected effective privileges.
	run env SUXEN_TOKEN="$token" "$SUXEN_EXAMPLES/.bin/suxenctl" whoami
	[ "$status" -eq 0 ]
	[[ "$output" == *"repository:app:write"* ]]
	[[ "$output" != *"repository:logs:write"* ]]
}

@test "anonymous access is read-only" {
	# Seed an asset with the admin token.
	run put_code app public/b.txt "$SUXEN_TOKEN"; [ "$output" = "201" ]

	# Anonymous can read because the explicit development profile grants repository:*:read ...
	run get_code app public/b.txt; [ "$output" = "200" ]
	# ... but not write (401, no credentials).
	run put_code app public/c.txt; [ "$output" = "401" ]
}
