# shellcheck shell=bash

fail_with_output() {
  local message="$1"
  printf '%s\n' "${message}" >&2
  if [[ -n "${output:-}" ]]; then
    printf '%s\n' "${output}" >&2
  fi
  return 1
}

fail() {
  fail_with_output "$1"
}

assert_success() {
  if [[ "${status}" -ne 0 ]]; then
    fail_with_output "expected command to succeed, status=${status}"
  fi
}

assert_failure() {
  if [[ "${status}" -eq 0 ]]; then
    fail_with_output "expected command to fail"
  fi
}

assert_equal() {
  local actual="$1"
  local expected="$2"
  if [[ "${actual}" != "${expected}" ]]; then
    printf 'expected: %s\nactual:   %s\n' "${expected}" "${actual}" >&2
    return 1
  fi
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    printf 'expected output to contain %q:\n%s\n' "${needle}" "${haystack}" >&2
    return 1
  fi
}

assert_http_status() {
  local expected="$1"
  assert_equal "${status}" 0
  assert_equal "${lines[0]}" "${expected}"
}

require_command() {
  local command_name="$1"
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    if [[ "${SUXEN_E2E_FULL:-0}" == "1" ]]; then
      fail "required full-interoperability command is missing: ${command_name}"
      return
    fi
    skip "${command_name} is not installed"
  fi
}

require_full_interop() {
  if [[ "${SUXEN_E2E_FULL:-0}" != "1" ]]; then
    skip "set SUXEN_E2E_FULL=1 to run real-client scenarios"
  fi
}

wait_until() {
  local description="$1"
  local attempts="$2"
  shift 2

  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if "$@"; then
      return 0
    fi
    sleep 1
  done
  printf 'timed out waiting for %s after %d attempts\n' "${description}" "${attempts}" >&2
  return 1
}
