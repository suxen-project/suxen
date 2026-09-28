# Capacity and availability

Capacity depends on the database, blob store, network, ingress, artifact mix, and memory
limit. The numbers below are a reproducible reference point, not a service-level target.
Run the [capacity harness](../../test/capacity/README.md) on the intended infrastructure
before choosing production limits.

## Reference baseline

The checked-in 2026-09-20 baseline records its exact commit and environment under
[`test/capacity/baseline`](../../test/capacity/baseline). It used Docker 29.8.0 on
Linux/amd64, PostgreSQL 17, MinIO, and a proxy adding 5 ms before every object-store
request. Each topology started with empty volumes. Each replica had a 1 GiB memory limit.
The Raw phase sent 48 unique 1 MiB PUTs at concurrency 8. The npm phase sent four unique
publishes at concurrency 4; each carried a 45 MiB attachment in an approximately 60 MiB
JSON request.

| Topology and phase | Requests/s | Payload MiB/s | p95 | Maximum sampled RSS | Maximum sampled staging |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 replica, Raw | 12.77 | 12.77 | 1161 ms | 106.2 MiB | 5.0 MiB |
| 3 replicas, Raw | 23.11 | 23.11 | 1284 ms | 287.7 MiB summed | 9.0 MiB summed |
| 1 replica, npm | 0.87 | 52.32 | 3804 ms | 760.5 MiB | 199.5 MiB |
| 3 replicas, npm | 1.57 | 94.08 | 2168 ms | 775.9 MiB summed | 135.1 MiB summed |

RSS and staging are periodic samples, not process high-water marks. The sampler targets a
0.2-second interval, but sequential `docker exec` calls add overhead, especially with
three replicas. The success/OOM container states are stronger memory-bound evidence than
the sampled RSS values.

All requests succeeded and no replica restarted or was OOM-killed. PostgreSQL reported
1,531 commits, 0 rollbacks, 279 blocks read, 29,842 block hits, no temporary files, and
no deadlocks for the one-replica run. It reported 1,714 commits, 2 rollbacks, 285 blocks
read, 34,537 block hits, no temporary files, and no deadlocks for the three-replica run.
These counters include startup and provisioning.

After the Raw assets were deleted, immediate garbage collection completed in 550 ms with
one replica and 421 ms with three. The harness did not run uploads during GC, exercise an
online blob-store drain or migration, inject database latency, or remove replicas under
load. Existing deterministic integration tests cover serialization and failover for those
operations; they are not capacity evidence.

An explicit run at the previous 256 MiB chart limit sent the same four concurrent npm
requests to one replica. Docker reported `OOMKilled=true` and exit code 137; all four
requests failed before receiving an HTTP response. The same workload succeeded under the
new 1 GiB chart limit. This is why the chart no longer defaults to 256 MiB. Reproduce that
boundary with:

```sh
CAPACITY_MEMORY_LIMIT=256m ./test/capacity/run.sh
# Expected: nonzero exit; the result directory retains the container state.
```

## Upload and timeout bounds

`SUXEN_MAX_UPLOAD_BYTES` is the server-wide hard request or staged-upload bound and
defaults to 10 GiB. npm and Cargo publish parsers impose a lower 64 MiB request-body bound
because their JSON or framing is buffered. For npm, base64 and JSON overhead mean the
tarball payload must be smaller than 48 MiB; metadata reduces it further. PyPI and other
formats remain subject to the configured server-wide bound. Ingress and blob-store limits
may be lower and must be aligned explicitly.

Format index hooks also buffer metadata separately from the upload limit. PyPI's
`simple/` root source has a 128 MiB per-member bound, covering the roughly 42 MiB
JSON and 44 MiB HTML roots served by pypi.org in September 2026 and the larger
rewritten response of a Suxen proxy. High-churn npm packuments also have a
128 MiB source bound; other buffered format sources, including PyPI project
pages and npm non-packument paths, retain an
8 MiB bound. Hosted, rewritten, merged, and normalized PyPI root and project
output has a 128 MiB bound in either HTML or JSON. URL expansion and encoded
output are counted before allocation, including repeated request hosts and
HTML base URLs. A PyPI group accepts at most 256 MiB of normalized index
sources in total. These bounds are fixed and independent of
`SUXEN_MAX_UPLOAD_BYTES`. An index above them fails to render.

An npm group accepts at most 256 MiB of packument sources, and a rewritten
proxy or group packument is capped at 256 MiB. A direct hosted packument is
capped at 128 MiB; each published version's metadata record remains capped at
8 MiB. The npm hooks count JSON output before allocating the final serialized
body, including repeated rewritten tarball URLs. A September 2026 replay of
the 68,510,937-byte full `@prisma/client` packument served 69,012,662-byte
proxy and one-member group responses successfully. That single-process test
peaked at 1,190,664 KiB RSS (about 1.14 GiB), including its upstream fixture
and HTTP client buffers; it is not a server-only memory measurement. A hermetic
multi-version packument just above 8 MiB peaked at 168,676 KiB in the same
style of test.

Root rewrite and group merge hold input, parsed project names, and output in
memory at the same time. PyPI root/project proxy rewrites, npm packument
rewrites, and their final group merges share one render slot per replica,
separately from upload staging slots. Direct hosted rendering does not use
this slot.
Group source fetching and normalization happen before that slot is acquired,
and npm tarball resolution and stored group-owner checks also read packuments
outside it. Concurrent requests can therefore retain source buffers; the
wire-size caps and render slot do not impose a global RSS bound. A local
integration test with 679,640 projects and a
44,176,643-byte JSON root peaked at about 1.19 GiB process RSS while serving
both its proxy and one-member group; that process also held the test client and
fixture buffers, so it is not a server-only capacity measurement. The
reference npm capacity run above did not exercise large PyPI roots. Test a
representative root and group member count under the intended replica memory
limit before relying on the 1 GiB chart default.

The default `SUXEN_MAX_CONCURRENT_UPLOADS=4` is enforced per replica. It bounds concurrent
upload parsing and staging, including proxy cache fills after upstream response headers
arrive. It does not make the memory cost of a request streaming: an npm
publish holds the request JSON, decoded structures, base64 text, and decoded tarball while
it is processed. The 1 GiB chart limit was exercised with four approximately 60 MiB npm
bodies. If the memory limit is lower, the npm bodies are larger, or concurrency is raised,
measure that exact combination. A limit is a rejection threshold, not a promise that an
arbitrary number of maximum-sized uploads fits in memory or staging storage.

The default request read and response write deadlines are disabled so large streaming
uploads and downloads can take longer than five minutes. Request headers still have a
10-second deadline. Proxy connections have a 10-second dial and TLS handshake bound and
a 30-second response-header wait controlled by `SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT`;
the response body can stream until the caller cancels.
OIDC and webhook requests retain a 30-second overall deadline. Operators can set positive
`SUXEN_READ_TIMEOUT` and `SUXEN_WRITE_TIMEOUT` values to enforce overall transfer deadlines.
Ingress timeouts and body limits must allow the intended transfer size and duration.

## What replicas provide

Multiple replicas improve process availability, spread reads, and distribute request
parsing and staging. They do not create independent write lanes to one physical blob
store. Suxen acquires one database-backed lease per physical store for the blob `Put`
operations and metadata publication. Garbage collection and store migration use the same
lease. Those operations serialize for that store across every replica. This small run's
one/three throughput ratios are host observations, not scaling evidence; four npm requests
are enough for the memory boundary check, not a scaling conclusion.

Availability also depends on shared PostgreSQL and blob-store availability. A warm,
immutable cached blob can be served without its upstream. Mutable metadata, indexes, and
tagged manifests are revalidated after their TTL. If that revalidation fails, Suxen
returns the upstream error; it does not serve expired metadata. Do not treat a proxy
repository as a complete offline resolver unless every requested path is an unexpired or
immutable cache hit.
An npm, Cargo, or PyPI group also consults member package metadata before selecting an
artifact's source. Expired metadata must revalidate successfully even when its bytes
are cached; an unavailable source returns an error rather than a package from a
different member.
