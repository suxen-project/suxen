# Checked-in baseline evidence

Each dated directory contains the compact evidence behind the numeric table in
[`docs/operations/capacity.md`](../../../docs/operations/capacity.md). The successful
`one-three/` run comes from the default harness workload. The `memory-256m/` run uses the
same npm workload with `CAPACITY_MEMORY_LIMIT=256m` and is expected to exit nonzero after
the one-replica npm phase.

The environment file identifies the measured source commit. An empty Git status has the
SHA-256 digest `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
Request results, resource sample maxima, PostgreSQL counters, GC timing, and normalized
container states are retained. High-frequency TSV samples and full Prometheus scrapes are
host-specific bulk output and are deliberately omitted.
