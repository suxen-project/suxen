# Cleanup policy (retention)

A cleanup policy deletes assets that match ALL of its criteria, keeping the
newest `keepLast`. It runs **on demand** (an admin trigger) or on a schedule —
never automatically on upload — so retention is explicit and previewable.

## Server side

```sh
examples/cleanup/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml), which attaches a `raw-cleanup` policy to the `raw`
repository: under the `cleanup/` prefix, keep only the newest asset
(`keepLast: 1`, `action: delete`).

## Client / operator side

Preview first (a dry run lists what would be deleted), then apply:

```sh
suxenctl cleanup raw raw-cleanup           # dry run: previews wouldDelete
suxenctl cleanup --apply raw raw-cleanup   # performs the deletion
```

Cleanup removes asset metadata; the underlying blob bytes are reclaimed
separately by garbage collection:

```sh
suxenctl gc                                # dry run
suxenctl gc --apply
```

## Test

[`test.bats`](test.bats) uploads three assets under `cleanup/`, runs the policy
with `--apply`, and asserts the two oldest return `404` while the newest still
returns `200`. Hermetic (suxenctl + a pinned `curl` image); needs only Docker.
Run it with `bats examples/cleanup/test.bats`.
