#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

@test "Raw assets deduplicate identical content in MinIO" {
  printf 'shared content-addressed bytes\n' >"${E2E_CASE_DIRECTORY}/shared.txt"
  local before
  before="$(minio_object_count)"

  raw_put raw dedup/one.txt "${E2E_CASE_DIRECTORY}/shared.txt" >/dev/null
  raw_put raw dedup/two.txt "${E2E_CASE_DIRECTORY}/shared.txt" >/dev/null

  local after
  after="$(minio_object_count)"
  assert_equal "$((after - before))" "1"
}

@test "Raw asset listing honors a path prefix" {
  run api GET '/api/v1/repositories/raw/assets?prefix=dedup/'
  assert_success
  run jq -e '
    [.items[].path] as $paths |
    ($paths | index("dedup/one.txt")) and
    ($paths | index("dedup/two.txt")) and
    ($paths | all(startswith("dedup/")))
  ' <<<"${output}"
  assert_success
}
