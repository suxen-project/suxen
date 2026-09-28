#!/usr/bin/env bats
# Boots the example (verify-on-push trust policy on the raw repo). The first
# test proves an unsigned upload is rejected against the shipped policy
# (hermetic). The second registers a fresh key and pushes a correctly signed
# asset — the real operator workflow — and needs openssl (skips without it).

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

@test "verify-on-push rejects an unsigned upload" {
	require_docker
	printf 'unsigned\n' >"$SUXEN_EXAMPLE_DATA/u.txt"
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/u.txt "$SUXEN_URL/repository/raw/prov/u.txt"
	[ "$status" -eq 0 ]
	[ "$output" = "403" ]
}

@test "a correctly signed upload is accepted after registering the key" {
	require_openssl
	local d="$SUXEN_EXAMPLE_DATA"
	# Generate an EC keypair and register the public key on the operator-managed
	# `signed` repository (raw's policy is provisioned, so the API refuses edits).
	openssl ecparam -name prime256v1 -genkey -noout -out "$d/priv.pem" 2>/dev/null
	openssl ec -in "$d/priv.pem" -pubout -out "$d/pub.pem" 2>/dev/null
	# Fold the PEM into a JSON string (newlines -> \n) without extra tooling.
	local pem; pem="$(sed ':a;N;$!ba;s/\n/\\n/g' "$d/pub.pem")"
	printf '{"mode":"verify-on-push","publicKeys":["%s"]}\n' "$pem" >"$d/policy.json"
	suxenctl trust-policy set signed "$d/policy.json" >/dev/null

	# Sign the asset's digest string (sha256:<hex>) — that is the payload suxen
	# verifies when no explicit payload is supplied.
	printf 'signed artifact\n' >"$d/art.txt"
	local hex; hex="$(sha256sum "$d/art.txt" | cut -d' ' -f1)"
	printf '%s' "sha256:$hex" | openssl dgst -sha256 -sign "$d/priv.pem" >"$d/sig.der"

	# suxenctl raw put base64-encodes the binary signature into X-Suxen-Signature.
	run suxenctl raw put --signature "$d/sig.der" signed prov/art.txt "$d/art.txt"
	[ "$status" -eq 0 ]

	run client "$CURL" -sS -o /dev/null -w '%{http_code}' "$SUXEN_URL/repository/signed/prov/art.txt"
	[ "$output" = "200" ]
}
