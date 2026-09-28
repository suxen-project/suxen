#!/usr/bin/env bats
# Boots the example, then exercises suxenctl apply semantics: dry-run, prune,
# managed-resource protection + force, and secretRef from env and file.
# Hermetic: suxenctl + curl, no network.

load "../lib/harness.bash"

setup_file() { suxen_start "$BATS_TEST_DIRNAME"; }
teardown_file() { suxen_stop; }

CURL="curlimages/curl:8.11.1"

repo_exists() { suxenctl repo list | grep -q "\"name\":\"$1\""; }
user_exists() { suxenctl user list | grep -q "\"username\":\"$1\""; }

with_beta() {
	cat >"$SUXEN_EXAMPLE_DATA/with-beta.yaml" <<-EOF
		apiVersion: suxen.io/v1
		resources:
		  - {kind: repository, name: alpha, spec: {format: raw, type: hosted}}
		  - {kind: repository, name: beta, spec: {format: raw, type: hosted}}
	EOF
	echo "$SUXEN_EXAMPLE_DATA/with-beta.yaml"
}
alpha_only() {
	cat >"$SUXEN_EXAMPLE_DATA/alpha-only.yaml" <<-EOF
		apiVersion: suxen.io/v1
		resources:
		  - {kind: repository, name: alpha, spec: {format: raw, type: hosted}}
	EOF
	echo "$SUXEN_EXAMPLE_DATA/alpha-only.yaml"
}

@test "dry-run reports changes without applying them" {
	run suxenctl apply -f "$(with_beta)" --dry-run
	[ "$status" -eq 0 ]
	[[ "$output" == *'"name":"beta"'* ]]
	[[ "$output" == *'"dryRun":true'* ]]
	run repo_exists beta; [ "$status" -ne 0 ]   # not actually created
}

@test "apply is idempotent, and prune removes owned resources omitted from the document" {
	suxenctl apply -f "$(with_beta)" >/dev/null
	run repo_exists beta; [ "$status" -eq 0 ]

	# Re-applying the same document changes nothing.
	run suxenctl apply -f "$(with_beta)"
	[ "$status" -eq 0 ]
	[[ "$output" == *'"status":"unchanged"'* ]]

	# Applying a document that omits beta, with --prune, deletes beta; the
	# bootstrap repositories (alpha, raw, oci) are retained.
	suxenctl apply -f "$(alpha_only)" --prune >/dev/null
	run repo_exists beta; [ "$status" -ne 0 ]
	run repo_exists alpha; [ "$status" -eq 0 ]
	run repo_exists raw; [ "$status" -eq 0 ]
}

@test "a managed resource rejects ad-hoc edits unless forced" {
	run suxenctl repo delete alpha
	[ "$status" -ne 0 ]
	[[ "$output" == *"managed_resource"* ]]

	# force=true overrides the guard.
	run client "$CURL" -sS -o /dev/null -w '%{http_code}' -X DELETE \
		-H "Authorization: Bearer $SUXEN_TOKEN" \
		"$SUXEN_URL/api/v1/repositories/alpha?force=true"
	[ "$output" = "204" ]
	run repo_exists alpha; [ "$status" -ne 0 ]
}

@test "secretRef resolves a user's password from env and from a file" {
	printf 'file-password-123\n' >"$SUXEN_EXAMPLE_DATA/fsecret.txt"
	cat >"$SUXEN_EXAMPLE_DATA/users.yaml" <<-EOF
		apiVersion: suxen.io/v1
		resources:
		  - {kind: user, name: from-env,  spec: {admin: false, roles: [], secretRef: {env: PROV_ENV_SECRET}}}
		  - {kind: user, name: from-file, spec: {admin: false, roles: [], secretRef: {file: $SUXEN_EXAMPLE_DATA/fsecret.txt}}}
	EOF
	# Secrets are resolved where apply runs: env vars and absolute file paths.
	run env PROV_ENV_SECRET=env-password-123 \
		"$SUXEN_EXAMPLES/.bin/suxenctl" apply -f "$SUXEN_EXAMPLE_DATA/users.yaml"
	[ "$status" -eq 0 ]
	run user_exists from-env;  [ "$status" -eq 0 ]
	run user_exists from-file; [ "$status" -eq 0 ]
}

@test "apply preserves exact numeric predicates and omitted fields" {
	cat >"$SUXEN_EXAMPLE_DATA/exact-policy.yaml" <<-EOF
		apiVersion: suxen.io/v1
		resources:
		  - {kind: repository, name: exact-policy, spec: {format: raw, type: hosted}}
		  - kind: downloadGate
		    name: exact-policy
		    spec:
		      enabled: false
		      criteria:
		        - {path: scan.serial, op: "=", value: 9007199254740993}
	EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/exact-policy.yaml" >/dev/null
	run suxenctl download-gate get exact-policy
	[ "$status" -eq 0 ]
	[[ "$output" == *'"value":9007199254740993'* ]]

	cat >"$SUXEN_EXAMPLE_DATA/enable-policy.yaml" <<-EOF
		resources:
		  - {kind: downloadGate, name: exact-policy, spec: {enabled: true}}
	EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/enable-policy.yaml" >/dev/null
	run suxenctl download-gate get exact-policy
	[ "$status" -eq 0 ]
	[[ "$output" == *'"value":9007199254740993'* ]]
	[[ "$output" == *'"enabled":true'* ]]

	cat >"$SUXEN_EXAMPLE_DATA/change-policy.yaml" <<-EOF
		resources:
		  - kind: downloadGate
		    name: exact-policy
		    spec:
		      criteria:
		        - {path: scan.serial, op: "=", value: 9007199254740995}
	EOF
	suxenctl apply -f "$SUXEN_EXAMPLE_DATA/change-policy.yaml" >/dev/null
	run suxenctl download-gate get exact-policy
	[ "$status" -eq 0 ]
	[[ "$output" == *'"value":9007199254740995'* ]]
	[[ "$output" == *'"enabled":true'* ]]
}

@test "invalid YAML numbers reject the complete document before mutation" {
	cat >"$SUXEN_EXAMPLE_DATA/invalid-policy.yaml" <<-EOF
		resources:
		  - {kind: repository, name: invalid-policy, spec: {format: raw, type: hosted}}
		  - kind: downloadGate
		    name: invalid-policy
		    spec:
		      enabled: true
		      criteria:
		        - {path: scan.score, op: "<", value: .nan}
	EOF
	run suxenctl apply -f "$SUXEN_EXAMPLE_DATA/invalid-policy.yaml"
	[ "$status" -ne 0 ]
	run repo_exists invalid-policy
	[ "$status" -ne 0 ]
}
