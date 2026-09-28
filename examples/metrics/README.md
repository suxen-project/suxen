# Prometheus metrics

suxen exposes Prometheus/OpenMetrics text at `GET /metrics`, through the official
Prometheus Go client. It is not public: a scraper must present a bearer token
whose subject has `admin:stats:read` (the built-in `administrator` role includes
it), so metrics are not readable by ordinary repository users.

## Server side

```sh
examples/metrics/start.sh
```

Applies [`repo.yaml`](repo.yaml): a `files` repository (so an upload produces
traffic to report) and a non-admin `reader` role used to show the auth gate.

## Client / operator side

```sh
# Anonymous: 401. A token without admin:stats:read: 403.
curl "$SUXEN_URL/metrics"                                            # 401
curl -H "Authorization: Bearer $SUXEN_TOKEN" "$SUXEN_URL/metrics"    # 200
```

A Prometheus scrape config points at the endpoint with a bearer-token file:

```yaml
scrape_configs:
  - job_name: suxen
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/suxen-token
    static_configs:
      - targets: ["suxen:8080"]
```

(The Helm chart wires a `ServiceMonitor` and the token Secret it references; see
the chart guide.)

## What it exposes

Besides the default `go_*` / `process_*` collectors, suxen publishes its own
series, including:

- `suxen_http_requests_total`, `suxen_http_request_duration_seconds_*` — request
  counts and latency, labelled by bounded method/status/format/repository;
- `suxen_blob_operations_total`, `suxen_blob_bytes`, `suxen_unique_blobs` — blob
  store activity and totals;
- `suxen_proxy_cache_requests_total` — proxy cache hits/misses;
- `suxen_authentication_failures_total`, `suxen_authentication_blocked_total`;
- `suxen_webhook_deliveries_total`, `suxen_webhook_queue`,
  `suxen_webhook_dead_letters` — webhook delivery health;
- `suxen_repositories`, `suxen_assets`, `suxen_uptime_seconds`, `suxen_leader`.

The full reference is in [docs/reference/metrics.md](../../docs/reference/metrics.md).

## Test

[`test.bats`](test.bats) asserts the auth gate (anonymous 401, non-admin token
403, admin token 200) and that an admin scrape after an upload contains
`suxen_http_requests_total`, `suxen_blob_operations_total`, `suxen_repositories`,
and `suxen_uptime_seconds`. Hermetic: suxenctl (admin) + a pinned curl image;
needs only Docker. Run it with `bats examples/metrics/test.bats`.
