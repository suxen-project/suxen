#!/usr/bin/env bats
# Boots the OIDC example (a Dex IdP in a compose stack + a suxen federated to it)
# and exercises the browser-free OAuth2 password grant: an IdP-only user
# authenticates against suxen, receives the provider's defaultRoles, and writes
# to a repository no anonymous user may write to.

load "../lib/harness.bash"

setup_file() {
	# The oidcProvider's client secret is a secretRef resolved at apply time, so
	# it must be present when the harness re-applies repo.yaml (and it matches
	# what start.sh sets for the launcher's own apply).
	export SUXEN_OIDC_CLIENT_SECRET="suxen-oidc-client-secret"
	suxen_start "$BATS_TEST_DIRNAME"
}
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"
ALICE="alice@example.com:alice-password"

@test "an IdP user with no local account writes via the password grant" {
	require_docker
	run client "$CURL" -s -o /dev/null -w '%{http_code}' \
		-u "$ALICE" -X PUT --data 'from-alice' "$SUXEN_URL/repository/app/alice.txt"
	[ "$output" = "201" ]

	run client "$CURL" -fsS -u "$ALICE" "$SUXEN_URL/repository/app/alice.txt"
	[ "$status" -eq 0 ]
	[ "$output" = "from-alice" ]
}

@test "a wrong IdP password is refused" {
	require_docker
	run client "$CURL" -s -o /dev/null -w '%{http_code}' \
		-u "alice@example.com:wrong-password" \
		-X PUT --data 'nope' "$SUXEN_URL/repository/app/bad.txt"
	[ "$output" = "401" ]
}

@test "anonymous cannot write, but keeps the development-profile read grant" {
	require_docker
	run client "$CURL" -s -o /dev/null -w '%{http_code}' \
		-X PUT --data 'anon' "$SUXEN_URL/repository/app/anon.txt"
	[ "$output" = "401" ]

	# The first test wrote alice.txt; anonymous may read it (repository:*:read).
	run client "$CURL" -s -o /dev/null -w '%{http_code}' \
		"$SUXEN_URL/repository/app/alice.txt"
	[ "$output" = "200" ]
}

@test "whoami reflects the OIDC-derived identity and role" {
	require_docker
	run client "$CURL" -fsS -u "$ALICE" "$SUXEN_URL/api/v1/whoami"
	[ "$status" -eq 0 ]
	# Authenticated through the IdP, not a local account.
	echo "$output" | grep -q '"authenticationKind":"oidc-password"'
	echo "$output" | grep -q '"username":"alice@example.com"'
	# The provider's defaultRoles granted the write privilege.
	echo "$output" | grep -q '"oidc-writer"'
	echo "$output" | grep -q 'repository:app:write'
}
