# Capacity baseline harness

This opt-in harness records a small, repeatable one-versus-three-replica baseline. It is
an operational sizing aid, not a CI performance threshold or a production capacity
guarantee. Both topologies use PostgreSQL, one S3-compatible MinIO store, and a proxy that
adds a fixed delay to every object-store request. Each Suxen container has the Helm
chart's default 1 GiB memory limit.

From the repository root, run it on an otherwise idle Linux Docker host:

```sh
./test/capacity/run.sh
```

The default workload is bounded:

| Phase | Requests | Payload per request | Concurrency |
| --- | ---: | ---: | ---: |
| Raw PUT | 48 | 1 MiB | 8 |
| npm publish | 4 | 45 MiB attachment (about 60 MiB JSON body) | 4 |

The object-store proxy adds 5 ms before every MinIO request. Override the values with
`CAPACITY_RAW_COUNT`, `CAPACITY_RAW_SIZE`, `CAPACITY_RAW_CONCURRENCY`,
`CAPACITY_NPM_COUNT`, `CAPACITY_NPM_SIZE`, `CAPACITY_NPM_CONCURRENCY`, and
`CAPACITY_STORAGE_LATENCY`. `CAPACITY_MEMORY_LIMIT` overrides the per-replica memory
limit. Sizes are bytes and latency is a Go duration such as `20ms`. `CAPACITY_RESULTS`
selects the output directory. Set `CAPACITY_KEEP=true` to retain the last Compose project
for inspection.

Each topology starts with empty volumes. The runner records request throughput and
p50/p95/max latency, maximum sampled summed replica RSS, summed replica staging usage (the data
directory plus the S3 driver's temporary files),
PostgreSQL counters, Prometheus metrics, container state (including OOM status), and an
immediate garbage-collection duration after deleting the measured Raw assets. It also
records the exact commit and dirty-state hash, workload, CPU, host and cgroup memory,
kernel, Go version, and Docker version. RSS and staging samples target a 0.2-second
interval; sequential `docker exec` calls make the effective interval longer. Results
under `test/capacity/results/` are ignored because host measurements are not portable.

The harness does not simulate database latency, S3 bandwidth limits, cross-zone network
loss, ingress buffering, rolling drain, or online blob-store migration. Measure those on
the intended infrastructure before setting production limits.
