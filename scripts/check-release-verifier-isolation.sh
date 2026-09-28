#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT
events="${work_directory}/events"

cat >"${work_directory}/tool-stub" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
tool="$(basename "$0")"
printf '%s\t%s\n' "${tool}" "$*" >>"${VERIFIER_ISOLATION_EVENTS}"
if [[ "${tool}" == curl ]]; then
  previous=""
  for argument in "$@"; do
    if [[ "${previous}" == --output ]]; then
      : >"${argument}"
    fi
    previous="${argument}"
  done
elif [[ "${tool}" == docker && "${1:-}" == buildx ]]; then
  printf '{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}}]}\n'
elif [[ "${tool}" == helm && "${1:-}" == install && "${FAIL_HELM_INSTALL:-}" == true ]]; then
  exit 1
elif [[ "${tool}" == kubectl && "${1:-}" == create && "${FAIL_NAMESPACE_CREATE:-}" == true ]]; then
  exit 1
fi
EOF
chmod +x "${work_directory}/tool-stub"
for tool in cosign curl docker gh helm kubectl; do
  ln -s tool-stub "${work_directory}/${tool}"
done

run_verifier() {
  PATH="${work_directory}:${PATH}" \
    VERIFIER_ISOLATION_EVENTS="${events}" \
    SUXEN_RELEASE_ARTIFACT_CHECKER=/bin/true \
    SUXEN_RELEASE_EXECUTE_LINUX_TARGETS=native \
    FAIL_HELM_INSTALL="${FAIL_HELM_INSTALL:-}" \
    FAIL_NAMESPACE_CREATE="${FAIL_NAMESPACE_CREATE:-}" \
    "${root_dir}/scripts/verify-published-release.sh" \
    1.0.0-rc.1 suxen-project/suxen >/dev/null 2>&1
}

# A partial Helm install may fail, but cleanup must remove only the unique
# namespace this invocation successfully created.
: >"${events}"
if FAIL_HELM_INSTALL=true run_verifier; then
  printf 'partial Helm install unexpectedly succeeded\n' >&2
  exit 1
fi
release_name="$(awk -F '\t' '$1 == "helm" && $2 ~ /^install / {print $2}' "${events}" | awk '{print $2}')"
namespace="$(awk -F '\t' '$1 == "kubectl" && $2 ~ /^create namespace / {print $2}' "${events}" | awk '{print $3}')"
[[ "${release_name}" == suxen-verify-* && "${release_name}" == "${namespace}" ]]
[[ "${release_name}" != suxen-release-verification ]]
grep -q $'^kubectl\tdelete namespace '"${namespace}"' --ignore-not-found=true --wait=false$' "${events}"

# If namespace creation collided or failed, ownership was never acquired and
# the trap must not delete that namespace.
: >"${events}"
if FAIL_NAMESPACE_CREATE=true run_verifier; then
  printf 'failed namespace creation unexpectedly succeeded\n' >&2
  exit 1
fi
if grep -q $'^kubectl\tdelete namespace ' "${events}"; then
  printf 'verifier deleted a namespace it did not create\n' >&2
  exit 1
fi

# Concurrent invocations must never share a Helm release or namespace name.
: >"${events}"
FAIL_HELM_INSTALL=true run_verifier & first_pid=$!
FAIL_HELM_INSTALL=true run_verifier & second_pid=$!
wait "${first_pid}" || true
wait "${second_pid}" || true
mapfile -t names < <(
  awk -F '\t' '$1 == "helm" && $2 ~ /^install / {print $2}' "${events}" |
    awk '{print $2}' | sort -u
)
if [[ "${#names[@]}" -ne 2 ]]; then
  printf 'concurrent verifiers did not use two unique names: %s\n' "${names[*]:-none}" >&2
  exit 1
fi
