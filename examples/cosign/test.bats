#!/usr/bin/env bats
# Boots the example and exercises Sigstore cosign provenance: an operator
# generates a cosign key pair, registers it in a verify-on-push trust policy, and
# suxen accepts only a correctly cosign-signed artifact. Hermetic (no
# transparency log): keys are local and signing uses --tlog-upload=false.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"
COSIGN="gcr.io/projectsigstore/cosign:v2.4.1"

# cosign runs the cosign CLI in its pinned image, on the host network, over the
# test's working files, with an empty key password (non-interactive).
cosign() {
	docker run --rm --network host --user "$(id -u):$(id -g)" \
		-v "$SUXEN_EXAMPLE_DATA:/work" -w /work -e HOME=/work -e COSIGN_PASSWORD="" \
		"$COSIGN" "$@"
}

@test "verify-on-push accepts only a correctly cosign-signed artifact" {
	require_docker
	local d="$SUXEN_EXAMPLE_DATA"

	# Operator generates a cosign key pair and registers the public key in a
	# verify-on-push policy on `releases`.
	cosign generate-key-pair --output-key-prefix /work/cosign >/dev/null
	local pem; pem="$(sed ':a;N;$!ba;s/\n/\\n/g' "$d/cosign.pub")"
	printf '{"mode":"verify-on-push","publicKeys":["%s"]}\n' "$pem" >"$d/policy.json"
	suxenctl trust-policy set releases "$d/policy.json" >/dev/null

	# An unsigned upload is rejected.
	printf 'release payload\n' >"$d/artifact.txt"
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $SUXEN_TOKEN" \
		--upload-file /work/artifact.txt "$SUXEN_URL/repository/releases/unsigned.txt"
	[ "$output" = "403" ]

	# cosign signs the artifact's digest string (sha256:<hex>) — the payload suxen
	# verifies when none is supplied. The base64 signature goes in X-Suxen-Signature.
	printf 'sha256:%s' "$(sha256sum "$d/artifact.txt" | cut -d' ' -f1)" >"$d/digest.txt"
	cosign sign-blob --key /work/cosign.key --output-signature /work/artifact.sig \
		--tlog-upload=false --yes /work/digest.txt >/dev/null 2>&1
	local sig; sig="$(tr -d '\r\n' <"$d/artifact.sig")"

	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $SUXEN_TOKEN" -H "X-Suxen-Signature: $sig" \
		--upload-file /work/artifact.txt "$SUXEN_URL/repository/releases/signed.txt"
	[ "$output" = "201" ]

	# verify-on-push released it, so it is served.
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		"$SUXEN_URL/repository/releases/signed.txt"
	[ "$output" = "200" ]

	# A signature from a second key the policy does not trust is rejected — proof
	# the policy verifies the signer, not merely the presence of a signature.
	cosign generate-key-pair --output-key-prefix /work/other >/dev/null
	cosign sign-blob --key /work/other.key --output-signature /work/other.sig \
		--tlog-upload=false --yes /work/digest.txt >/dev/null 2>&1
	local other; other="$(tr -d '\r\n' <"$d/other.sig")"
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $SUXEN_TOKEN" -H "X-Suxen-Signature: $other" \
		--upload-file /work/artifact.txt "$SUXEN_URL/repository/releases/badkey.txt"
	[ "$output" = "403" ]
}
