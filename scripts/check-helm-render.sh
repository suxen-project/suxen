#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root_dir}/charts/suxen"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fail() {
  printf 'Helm render assertion failed: %s\n' "$*" >&2
  exit 1
}

render() {
  local name="$1"
  shift
  helm template review "${chart}" \
    --set bootstrap.existingSecret=credentials \
    "$@" >"${work_dir}/${name}.yaml"
}

# The server has no Kubernetes API integration, so the pod must not receive
# a service-account token. Check the rendered Deployment rather than template
# text so a future chart change cannot silently restore Kubernetes' default.
helm template review "${chart}" \
  --show-only templates/deployment.yaml >"${work_dir}/deployment.yaml"
grep -q '^      automountServiceAccountToken: false$' "${work_dir}/deployment.yaml" ||
  fail "Deployment pod did not disable service-account token mounting"

data_volume() {
  awk '
    /^        - name: data$/ { capture = 1 }
    capture { print }
    capture && /^        - name: temporary$/ { exit }
  ' "$1"
}

# Keep the chart's supported environment surface in lockstep with the process
# configuration. A new SUXEN_* setting must be mapped deliberately rather than
# becoming available only to non-Helm deployments.
grep -oE 'SUXEN_[A-Z0-9_]+' "${root_dir}/internal/config/config.go" |
  sort -u >"${work_dir}/runtime-environment"
grep -oE 'SUXEN_[A-Z0-9_]+' "${chart}/templates/deployment.yaml" |
  sort -u >"${work_dir}/chart-environment"
if ! diff -u "${work_dir}/runtime-environment" "${work_dir}/chart-environment"; then
  fail "chart environment mappings differ from internal/config"
fi

# Moving only the blob store to S3 must retain the chart-managed claim because
# SQLite metadata still lives under SUXEN_DATA. This also protects upgrades from
# the default filesystem topology from deleting the existing PVC.
render sqlite-s3 --set blobStore.url=s3://artifacts
grep -q '^kind: PersistentVolumeClaim$' "${work_dir}/sqlite-s3.yaml" ||
  fail "SQLite with S3 did not render a PersistentVolumeClaim"
grep -q 'claimName: review-suxen' < <(data_volume "${work_dir}/sqlite-s3.yaml") ||
  fail "SQLite with S3 did not mount the chart-managed claim"
if grep -q 'name: SUXEN_DB' "${work_dir}/sqlite-s3.yaml"; then
  fail "SQLite with S3 unexpectedly configured an external database"
fi

# When both stateful services are external, /var/lib/suxen contains only
# disposable process data and no chart-managed claim is needed.
render postgres-s3 \
  --set database.existingSecret=database \
  --set blobStore.url=s3://artifacts
if grep -q '^kind: PersistentVolumeClaim$' "${work_dir}/postgres-s3.yaml"; then
  fail "PostgreSQL with S3 unexpectedly rendered a PersistentVolumeClaim"
fi
grep -q 'emptyDir: {}' < <(data_volume "${work_dir}/postgres-s3.yaml") ||
  fail "PostgreSQL with S3 did not render an ephemeral data volume"

# GCS is shared object storage just like S3. It must satisfy cluster validation,
# avoid a redundant PVC with PostgreSQL, and mount an optional service-account
# key through the driver's standard application-default-credentials variable.
render postgres-gcs \
  --set replicaCount=3 \
  --set config.cluster=true \
  --set database.existingSecret=database \
  --set blobStore.url=gcs://artifacts/suxen \
  --set blobStore.gcsCredentialsSecret=gcs-service-account
if grep -q '^kind: PersistentVolumeClaim$' "${work_dir}/postgres-gcs.yaml"; then
  fail "PostgreSQL with GCS unexpectedly rendered a PersistentVolumeClaim"
fi
grep -q 'emptyDir: {}' < <(data_volume "${work_dir}/postgres-gcs.yaml") ||
  fail "PostgreSQL with GCS did not render an ephemeral data volume"
grep -q 'value: /etc/suxen/gcs/credentials.json' "${work_dir}/postgres-gcs.yaml" ||
  fail "GCS credentials path was not configured"
grep -q 'secretName: gcs-service-account' "${work_dir}/postgres-gcs.yaml" ||
  fail "GCS credentials Secret was not mounted"
if helm template review "${chart}" \
  --set blobStore.url=gcs://artifacts \
  --set blobStore.credentialsSecret=s3-environment \
  --set blobStore.gcsCredentialsSecret=gcs-service-account \
  >"${work_dir}/invalid-mixed-credentials.yaml" 2>/dev/null; then
  fail "mixed S3 environment and GCS file credentials unexpectedly rendered"
fi

# Every operator-facing scheduler and webhook setting must render through the
# chart, including settings whose binary default disables the scheduler.
render runtime-config \
  --set bootstrap.user=release-admin \
  --set config.verifyInterval=6h \
  --set config.migrateInterval=2m \
  --set config.webhookRetryBase=3s \
  --set config.webhookPollInterval=4s \
  --set config.webhookMaxAttempts=12
for mapping in \
  'SUXEN_BOOTSTRAP_USER release-admin' \
  'SUXEN_VERIFY_INTERVAL 6h' \
  'SUXEN_MIGRATE_INTERVAL 2m' \
  'SUXEN_WEBHOOK_RETRY_BASE 3s' \
  'SUXEN_WEBHOOK_POLL_INTERVAL 4s' \
  'SUXEN_WEBHOOK_MAX_ATTEMPTS 12'; do
  read -r variable expected <<<"${mapping}"
  if ! awk -v variable="${variable}" -v expected="${expected}" '
    $0 ~ "^[[:space:]]*- name: " variable "$" { found = 1; next }
    found && $1 == "value:" {
      gsub(/^"|"$/, "", $2)
      if ($2 == expected) matched = 1
      exit
    }
    END { exit !matched }
  ' "${work_dir}/runtime-config.yaml"; then
    fail "${variable} did not render value ${expected}"
  fi
done
