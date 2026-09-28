#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT
artifacts="${work_directory}/artifacts"
mkdir -p "${artifacts}"
touch "${artifacts}/SHA256SUMS" "${work_directory}/bundle.json"
events="${work_directory}/events"

cat >"${work_directory}/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'cosign\n' >>"${RELEASE_ORDER_EVENTS}"
[[ "${FAIL_RELEASE_STAGE:-}" != cosign ]]
EOF
cat >"${work_directory}/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
asset="$(basename "$3")"
printf 'attest %s\n' "${asset}" >>"${RELEASE_ORDER_EVENTS}"
[[ "${FAIL_RELEASE_STAGE:-}" != "attest:${asset}" ]]
EOF
cat >"${work_directory}/checker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(sed -n '1p' "${RELEASE_ORDER_EVENTS}")" == cosign ]]
[[ "$(grep -c '^attest ' "${RELEASE_ORDER_EVENTS}")" -eq 8 ]]
printf 'execute\n' >>"${RELEASE_ORDER_EVENTS}"
EOF
chmod +x "${work_directory}/cosign" "${work_directory}/gh" "${work_directory}/checker"

run_verifier() {
  RELEASE_ORDER_EVENTS="${events}" \
    FAIL_RELEASE_STAGE="${FAIL_RELEASE_STAGE:-}" \
    COSIGN_BIN="${work_directory}/cosign" \
    GH_BIN="${work_directory}/gh" \
    SUXEN_RELEASE_ARTIFACT_CHECKER="${work_directory}/checker" \
    "${root_dir}/scripts/verify-release-downloads.sh" \
    1.0.0-rc.1 suxen-project/suxen "${artifacts}" "${work_directory}/bundle.json"
}

run_verifier
[[ "$(sed -n '1p' "${events}")" == cosign ]]
[[ "$(sed -n '10p' "${events}")" == execute ]]
[[ "$(wc -l <"${events}")" -eq 10 ]]

for failure in cosign attest:suxen-1.0.0-rc.1-linux-amd64.tar.gz; do
  : >"${events}"
  if FAIL_RELEASE_STAGE="${failure}" run_verifier >/dev/null 2>&1; then
    printf 'release authentication failure was ignored: %s\n' "${failure}" >&2
    exit 1
  fi
  if grep -q '^execute$' "${events}"; then
    printf 'artifact checker ran after failed authentication: %s\n' "${failure}" >&2
    exit 1
  fi
done

# The public verifier must delegate archive handling to the authenticated
# wrapper; a direct checker call would recreate the execute-before-auth bug.
if grep -q 'scripts/check-release-artifacts\.sh' \
  "${root_dir}/scripts/verify-published-release.sh"; then
  printf 'public verifier bypasses authenticated release-download wrapper\n' >&2
  exit 1
fi
