# Blob stores (named stores, binding, GC, draining)

Content is stored in named **blob stores**. An instance can have several; each
repository is bound to one. Unreferenced blobs are reclaimed by garbage
collection, and a store can be **drained** onto a replacement without downtime.
This example adds a second, filesystem-backed store (`archive`) alongside the
bootstrap `default`.

## Server side

```sh
examples/blobstores/start.sh
```

Builds and runs a local suxen and applies [`repo.yaml`](repo.yaml): the `archive`
blob store (driver `fs`, its URI resolved server-side from a `configurationRef`
env var) and a `cold` repository bound to it. `start.sh` sets `SUXEN_ARCHIVE_URI`
to a path under the data dir and a short `SUXEN_MIGRATE_INTERVAL` so drains
complete promptly.

## Client / operator side

A blob store's connection string is never inlined; it is resolved server-side
from an env var or file:

```yaml
- kind: blobStore
  name: archive
  spec:
    driver: fs
    configurationRef: { env: SUXEN_ARCHIVE_URI }   # e.g. fs:///var/lib/suxen/archive
- kind: repository
  name: cold
  spec: { format: raw, type: hosted, blobStore: archive }
```

Bytes uploaded to `cold` land in `archive`, not `default`. Inspect any store:

```sh
curl -H "Authorization: Bearer $SUXEN_TOKEN" \
  "$SUXEN_URL/api/v1/blob-stores/archive/usage"   # objectCount, totalBytes, referenced/unreferenced
```

Reclaim unreferenced blobs (a deleted asset leaves its blob orphaned):

```sh
suxenctl gc --apply --grace 0s    # grace controls the minimum age of what is collected
```

Retire a store onto a replacement — data migrates in the background, then the
source reports `drained`:

```sh
curl -X POST -H "Authorization: Bearer $SUXEN_TOKEN" -H 'Content-Type: application/json' \
  --data '{"target":"default"}' "$SUXEN_URL/api/v1/blob-stores/archive/drain"
```

## Test

[`test.bats`](test.bats) asserts: an upload to `cold` lands in `archive` and not
`default`; a deleted asset's blob becomes unreferenced and `gc --grace 0s`
reclaims it; and draining `archive` into `default` migrates its objects and marks
it `drained`. Hermetic (suxenctl + a pinned `curl` image); needs only Docker.
Run it with `bats examples/blobstores/test.bats`.
