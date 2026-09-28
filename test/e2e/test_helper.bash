# shellcheck shell=bash

load "${BATS_TEST_DIRNAME}/lib/assert.bash"
load "${BATS_TEST_DIRNAME}/lib/suxen.bash"

setup_e2e() {
  load_e2e_environment
  export E2E_CASE_DIRECTORY="${BATS_TEST_TMPDIR}/${BATS_TEST_FILENAME##*/}"
  mkdir -p "${E2E_CASE_DIRECTORY}"
}
