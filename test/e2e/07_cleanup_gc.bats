#!/usr/bin/env bats

load test_helper

setup() {
  setup_e2e
}

upload_cleanup_fixture() {
  local name="$1"
  printf 'cleanup fixture %s\n' "${name}" >"${E2E_CASE_DIRECTORY}/${name}.txt"
  raw_put raw "cleanup/releases/${name}.txt" "${E2E_CASE_DIRECTORY}/${name}.txt" >/dev/null
  sleep 1
}

@test "cleanup preview is non-mutating and keepLast retains the newest asset" {
  upload_cleanup_fixture one
  upload_cleanup_fixture two
  upload_cleanup_fixture three

  run api POST '/api/v1/repositories/raw/cleanup?policy=e2e-cleanup&dryRun=true'
  assert_success
  run jq -e '
    .dryRun == true and
    .status == "succeeded" and
    (.result.wouldDelete | length) == 2 and
    (.result.wouldDelete | index("cleanup/releases/three.txt") | not)
  ' <<<"${output}"
  assert_success

  run api_status GET /repository/raw/cleanup/releases/one.txt
  assert_success
  assert_equal "${output}" "200"

}

@test "lastAccessedBefore participates in cleanup predicate evaluation" {
  printf 'download timestamp fixture\n' >"${E2E_CASE_DIRECTORY}/downloaded.txt"
  raw_put raw cleanup/downloaded.txt "${E2E_CASE_DIRECTORY}/downloaded.txt" >/dev/null
  raw_get raw cleanup/downloaded.txt "${E2E_CASE_DIRECTORY}/read-back.txt"
  sleep 1

  local policy='{
    "name":"downloaded-assets",
    "repositories":["raw"],
    "criteria":[
      {"path":"classification.label","op":"=","value":"disposable"},
      {"path":"sys.lastAccessed","op":"before","value":"0s"}
    ],
    "keepLast":0,
    "action":"delete",
    "enabled":false
  }'
  run api POST /api/v1/cleanup-policies "${policy}"
  assert_success

  run api POST '/api/v1/repositories/raw/cleanup?policy=downloaded-assets&dryRun=true'
  assert_success
  run jq -e '.result.wouldDelete | index("cleanup/downloaded.txt")' <<<"${output}"
  assert_success
}

@test "garbage collection reclaims unreferenced bytes from MinIO" {
  printf 'garbage collection fixture\n' >"${E2E_CASE_DIRECTORY}/garbage.txt"
  raw_put raw cleanup/garbage.txt "${E2E_CASE_DIRECTORY}/garbage.txt" >/dev/null
  local before
  before="$(minio_object_count)"

  local garbage_asset
  garbage_asset="$(asset_id raw cleanup/garbage.txt)"
  api DELETE "/api/v1/repositories/raw/assets/${garbage_asset}" >/dev/null

  run api POST '/api/v1/gc?dryRun=true&grace=0s'
  assert_success
  run jq -e '.dryRun == true and (.wouldDelete | length) >= 1' <<<"${output}"
  assert_success

  run api POST '/api/v1/gc?dryRun=false&grace=0s'
  assert_success
  run jq -e '.dryRun == false and .deleted >= 1 and .reclaimedBytes > 0' <<<"${output}"
  assert_success

  local after
  after="$(minio_object_count)"
  if ((after >= before)); then
    fail "garbage collection did not reduce the MinIO object count: before=${before} after=${after}"
  fi
}
