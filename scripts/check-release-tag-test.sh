#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT

cat >"${work_directory}/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  "api repos/suxen-project/suxen/git/ref/tags/v1.0.0")
    if [[ "${TAG_KIND:-tag}" == tag ]]; then
      printf '{"object":{"type":"tag","sha":"tag-object"}}\n'
    else
      printf '{"object":{"type":"commit","sha":"tag-commit"}}\n'
    fi
    ;;
  "api repos/suxen-project/suxen/git/tags/tag-object")
    printf '{"verification":{"verified":%s},"object":{"type":"commit","sha":"tag-commit"}}\n' \
      "${TAG_VERIFIED:-true}"
    ;;
  "api repos/suxen-project/suxen/git/ref/heads/main --jq .object.sha")
    printf '%s\n' "${MAIN_COMMIT:-tag-commit}"
    ;;
  *)
    printf 'unexpected gh invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${work_directory}/gh"

run_check() {
  GH_BIN="${work_directory}/gh" \
    "${root_dir}/scripts/check-release-tag.sh" suxen-project/suxen v1.0.0
}

run_check >/dev/null

for failure in lightweight unverified wrong-commit; do
  case "${failure}" in
    lightweight)
      if TAG_KIND=commit run_check >/dev/null 2>&1; then
        printf 'lightweight release tag was accepted\n' >&2
        exit 1
      fi
      ;;
    unverified)
      if TAG_VERIFIED=false run_check >/dev/null 2>&1; then
        printf 'unverified release tag was accepted\n' >&2
        exit 1
      fi
      ;;
    wrong-commit)
      if MAIN_COMMIT=other-commit run_check >/dev/null 2>&1; then
        printf 'release tag outside main was accepted\n' >&2
        exit 1
      fi
      ;;
  esac
done
