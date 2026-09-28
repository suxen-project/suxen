#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

expected_digest='sha256:3dab5c9ec14fb3d6a1f0f6b1e28df5b1dfc52971135a78d9ac2b93f436613ccd'
fake_helm="${work_dir}/helm"
cat >"${fake_helm}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == push && "$2" == chart.tgz && "$3" == oci://registry.example ]]
case "${FAKE_HELM_MODE}" in
  stderr)
    printf 'Pushed: registry.example/chart:1.0.0-rc.1\n' >&2
    printf 'Digest: %s\n' "${FAKE_HELM_DIGEST}" >&2
    ;;
  stdout)
    printf 'Pushed: registry.example/chart:1.0.0-rc.1\n'
    printf 'Digest: %s\n' "${FAKE_HELM_DIGEST}"
    ;;
  duplicate)
    printf 'Digest: %s\n' "${FAKE_HELM_DIGEST}" >&2
    printf 'Digest: %s\n' "${FAKE_HELM_DIGEST}" >&2
    ;;
  malformed)
    printf 'Digest: sha256:short\n' >&2
    ;;
  failure)
    printf 'registry denied the chart push\n' >&2
    exit 42
    ;;
esac
EOF
chmod +x "${fake_helm}"

fail() {
  printf 'Helm push assertion failed: %s\n' "$*" >&2
  exit 1
}

for stream in stderr stdout; do
  log="${work_dir}/${stream}.log"
  digest="$(
    FAKE_HELM_MODE="${stream}" \
      FAKE_HELM_DIGEST="${expected_digest}" \
      HELM_BIN="${fake_helm}" \
      "${root_dir}/scripts/push-helm-chart.sh" chart.tgz oci://registry.example \
      2>"${log}"
  )"
  [[ "${digest}" == "${expected_digest}" ]] ||
    fail "digest from Helm ${stream} was ${digest}"
  grep -q '^Pushed: registry.example/chart:1.0.0-rc.1$' "${log}" ||
    fail "Helm ${stream} diagnostics were not replayed"
done

for invalid_mode in duplicate malformed; do
  if FAKE_HELM_MODE="${invalid_mode}" \
    FAKE_HELM_DIGEST="${expected_digest}" \
    HELM_BIN="${fake_helm}" \
    "${root_dir}/scripts/push-helm-chart.sh" chart.tgz oci://registry.example \
    >"${work_dir}/${invalid_mode}.out" 2>"${work_dir}/${invalid_mode}.err"; then
    fail "${invalid_mode} digest output was accepted"
  fi
done

set +e
FAKE_HELM_MODE=failure \
  FAKE_HELM_DIGEST="${expected_digest}" \
  HELM_BIN="${fake_helm}" \
  "${root_dir}/scripts/push-helm-chart.sh" chart.tgz oci://registry.example \
  >"${work_dir}/failure.out" 2>"${work_dir}/failure.err"
failure_status=$?
set -e
[[ "${failure_status}" -eq 42 ]] || fail "Helm failure status became ${failure_status}"
grep -q 'registry denied the chart push' "${work_dir}/failure.err" ||
  fail "Helm failure diagnostics were swallowed"
