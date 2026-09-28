#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
compose_file="$root/test/capacity/compose.yaml"
run_id=${CAPACITY_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
results=${CAPACITY_RESULTS:-$root/test/capacity/results/$run_id}
raw_count=${CAPACITY_RAW_COUNT:-48}
raw_size=${CAPACITY_RAW_SIZE:-1048576}
raw_concurrency=${CAPACITY_RAW_CONCURRENCY:-8}
npm_count=${CAPACITY_NPM_COUNT:-4}
npm_size=${CAPACITY_NPM_SIZE:-47185920}
npm_concurrency=${CAPACITY_NPM_CONCURRENCY:-4}
storage_latency=${CAPACITY_STORAGE_LATENCY:-5ms}
memory_limit=${CAPACITY_MEMORY_LIMIT:-1g}
token=suxen-capacity-admin-token-value
project="suxen-capacity-${run_id,,}"
project=${project//[^a-z0-9_-]/-}
binary=/tmp/suxen-capacity-$run_id
stop_file=$results/.stop-monitor

mkdir -p "$results"
export CAPACITY_STORAGE_LATENCY=$storage_latency
export CAPACITY_MEMORY_LIMIT=$memory_limit

compose() {
  docker compose --project-name "$project" -f "$compose_file" "$@"
}

cleanup() {
  touch "$stop_file"
  if [[ ${CAPACITY_KEEP:-false} != true ]]; then
    compose --profile three down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -f "$binary"
}
trap cleanup EXIT

for command in docker curl go awk sha256sum getconf; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

env GOCACHE="$results/go-build-cache" go build -o "$binary" "$root/test/capacity"
rm -rf "$results/go-build-cache"

cat >"$results/workload.env" <<EOF
CAPACITY_RAW_COUNT=$raw_count
CAPACITY_RAW_SIZE=$raw_size
CAPACITY_RAW_CONCURRENCY=$raw_concurrency
CAPACITY_NPM_COUNT=$npm_count
CAPACITY_NPM_SIZE=$npm_size
CAPACITY_NPM_CONCURRENCY=$npm_concurrency
CAPACITY_STORAGE_LATENCY=$storage_latency
CAPACITY_MEMORY_LIMIT=$memory_limit
EOF
{
  date -u +run_started=%Y-%m-%dT%H:%M:%SZ
  printf 'kernel='; uname -srmo
  printf 'cpu_count='; getconf _NPROCESSORS_ONLN
  awk -F ': ' '/^model name/ {print "cpu_model=" $2; exit}' /proc/cpuinfo
  awk '/^MemTotal:/ {print "host_mem_total_kib=" $2}' /proc/meminfo
  cgroup_path=$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)
  if [[ -n $cgroup_path && -r /sys/fs/cgroup$cgroup_path/memory.max ]]; then
    printf 'runner_cgroup_memory_max='; cat "/sys/fs/cgroup$cgroup_path/memory.max"
  else
    echo 'runner_cgroup_memory_max=unknown'
  fi
  docker version --format 'docker_client={{.Client.Version}} docker_server={{.Server.Version}}'
  docker info --format 'docker_cpus={{.NCPU}} docker_memory_bytes={{.MemTotal}}'
  go version
  printf 'git_head='; git -C "$root" rev-parse HEAD
  printf 'git_status_sha256='; git -C "$root" status --porcelain=v1 -uall | sha256sum | cut -d' ' -f1
  echo 'resource_sampling_target_interval=0.2s (docker exec overhead extends the effective interval)'
} >"$results/environment.txt"

monitor() {
  local topology=$1
  local output=$results/$topology-resources.tsv
  : >"$output"
  while [[ ! -e $stop_file ]]; do
    local rss=0 staging=0 id value
    while read -r id; do
      [[ -n $id ]] || continue
      value=$(docker exec "$id" awk '/^VmRSS:/ {print $2}' /proc/1/status 2>/dev/null || true)
      [[ $value =~ ^[0-9]+$ ]] || value=0
      rss=$((rss + value))
      # Content is staged under SUXEN_DATA/uploads and the S3 driver creates a
      # second temporary file in /tmp while it owns the per-store write lease.
      value=$(docker exec "$id" sh -c \
        "du -sk /var/lib/suxen /tmp 2>/dev/null | awk '{sum += \$1} END {print sum}'" \
        2>/dev/null || true)
      [[ $value =~ ^[0-9]+$ ]] || value=0
      staging=$((staging + value))
    done < <(compose ps -q suxen-a suxen-b suxen-c 2>/dev/null)
    printf '%s\t%s\t%s\n' "$(date +%s.%N)" "$rss" "$staging" >>"$output"
    sleep 0.2
  done
  awk 'BEGIN {r=0;s=0} {if ($2>r) r=$2; if ($3>s) s=$3} END {printf "{\"maximumSampledSummedReplicaRssKiB\":%d,\"maximumSampledSummedReplicaStagingKiB\":%d}\n",r,s}' \
    "$output" >"$results/$topology-resource-summary.json"
}

snapshot() {
  local topology=$1
  compose exec -T postgres psql -U suxen -d suxen -Atc \
    "select datname,numbackends,xact_commit,xact_rollback,blks_read,blks_hit,temp_files,temp_bytes,deadlocks from pg_stat_database where datname='suxen'" \
    >"$results/$topology-postgres.txt"
  for service in suxen-a suxen-b suxen-c; do
    local id
    id=$(compose ps -a -q "$service" 2>/dev/null || true)
    [[ -n $id ]] || continue
    docker inspect --format \
      '{"status":{{json .State.Status}},"running":{{.State.Running}},"oomKilled":{{.State.OOMKilled}},"restartCount":{{.RestartCount}},"exitCode":{{.State.ExitCode}}}' \
      "$id" >"$results/$topology-$service-state.json"
    if [[ $(docker inspect --format '{{.State.Running}}' "$id") == true ]]; then
      curl -fsS -H "Authorization: Bearer $token" "http://$(compose port "$service" 8080)/metrics" \
        >"$results/$topology-$service-metrics.txt" 2>/dev/null || true
    fi
  done
}

measure() {
  local topology=$1 phase=$2
  shift 2
  rm -f "$stop_file"
  monitor "$topology-$phase" &
  local monitor_pid=$!
  local status=0
  "$binary" "$@" >"$results/$topology-$phase.json" || status=$?
  touch "$stop_file"
  wait "$monitor_pid"
  rm -f "$stop_file"
  return "$status"
}

run_scenario() {
  local topology=$1 target service prefix
  compose --profile three down --volumes --remove-orphans >/dev/null 2>&1 || true
  if [[ $topology == one ]]; then
    compose up -d --build --wait suxen-a
    service=suxen-a
  else
    compose --profile three up -d --build --wait
    service=load-balancer
  fi
  target="http://$(compose port "$service" 8080)"
  prefix="capacity-$topology-$run_id"

  # Warm connections and the database outside the recorded request set.
  "$binary" raw -url "$target" -token "$token" -count 4 -size 65536 -concurrency 1 -prefix "$prefix-warm" \
    >"$results/$topology-warm.json"

  local load_status=0
  measure "$topology" raw raw -url "$target" -token "$token" -count "$raw_count" \
    -size "$raw_size" -concurrency "$raw_concurrency" -prefix "$prefix-raw" || load_status=$?
  if ((load_status != 0)); then
    snapshot "$topology"
    return "$load_status"
  fi
  measure "$topology" npm npm -url "$target" -token "$token" -count "$npm_count" \
    -size "$npm_size" -concurrency "$npm_concurrency" -prefix "$prefix-npm" || load_status=$?
  snapshot "$topology"
  if ((load_status != 0)); then
    return "$load_status"
  fi

  "$binary" delete -url "$target" -token "$token" -count "$raw_count" \
    -concurrency "$raw_concurrency" -prefix "$prefix-raw" >"$results/$topology-delete.json"
  local gc_started gc_finished gc_status=0
  gc_started=$(date +%s%N)
  curl -fsS -X POST -H "Authorization: Bearer $token" \
    "$target/api/v1/gc?dryRun=false&grace=0s" >"$results/$topology-gc-response.json" || gc_status=$?
  gc_finished=$(date +%s%N)
  printf '{"elapsedMillis":%.3f,"curlExitStatus":%d}\n' \
    "$((gc_finished-gc_started))e-6" "$gc_status" >"$results/$topology-gc-timing.json"
  return "$gc_status"
}

run_scenario one
run_scenario three

echo "capacity results: $results"
