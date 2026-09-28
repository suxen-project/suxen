# Metrics reference

`GET /metrics` exposes Prometheus and OpenMetrics text metrics through the official
Prometheus Go client. Scrapers must authenticate with a bearer token whose subject has
`admin:stats:read` (the built-in administrator role includes it).

## Exposed series

The endpoint includes the default `go_*` and `process_*` collectors plus:

- request counts and latency;
- proxy-cache results;
- blob operations;
- cleanup and garbage-collection deletions;
- authentication failures and lockouts;
- webhook outcomes, including current queue and dead-letter counts;
- SQL connection-pool state;
- local leader roles.

## Label cardinality

HTTP series use only bounded method, status, format, and repository labels. At most 100
repository names are retained by one process; additional names share the `overflow` label.
Artifact paths, digests, users, and secrets are never metric labels.

Metadata aggregate gauges such as `suxen_assets` refresh in the background every 30
seconds. A failed refresh retains the last successful values and increments
`suxen_metrics_aggregate_refresh_failures_total`; it does not make `/metrics` fail.

## Leader signal

`suxen_leader{role=...}` reports whether the scraped target holds an unexpired lease; the
Prometheus target's `instance` label identifies that replica without copying a random
holder ID into application labels. `suxen_leader_last_held_timestamp_seconds` retains
bounded cleanup-scheduler and provisioning history after a short lease ends.

A `ServiceMonitor` and the token Secret it references are configured through the Helm
chart; see the [Helm chart guide](../../charts/suxen/README.md).
