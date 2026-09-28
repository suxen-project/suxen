# Go modules

Three repositories that work together: `go-proxy` caches `proxy.golang.org`,
`go-hosted` accepts internal module uploads, and `go` (a group over both) is the
single GOPROXY that serves internal modules and cached public ones.

## Server side

```sh
examples/go/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop.

## Client side

Point the `go` toolchain at the group repository:

```sh
export GOAUTH="$HOME/.config/suxen/goauth" # emits Authorization: Bearer; see the consumer guide
export GOPROXY="$SUXEN_URL/repository/go"
export GOSUMDB=off               # or keep it on; suxen forwards sumdb/ upstream
go mod download rsc.io/quote@v1.5.2
```

`go-proxy` caches module files for good and re-reads version lists on the proxy
TTL. For internal modules, upload the generated `.info`/`.mod`/`.zip` for a
canonical version to `go-hosted` with a token that has
`repository:go-hosted:write`; the `go` group then serves them alongside public
modules, and `@v/list`/`@latest` are synthesized once all three files for a
version have been uploaded. The individual files are readable before then.
For `@latest`, releases take precedence over prereleases and pseudo-versions;
pseudo-versions are ordered by commit time.

Cleanup policies with `keepLast` retain complete module versions, counting a
version's `.info`, `.mod`, and `.zip` as one unit. The policy must match all
three files to delete a version. Incomplete uploads or cache entries are left
for explicit deletion.

## Test

[`test.bats`](test.bats) covers both halves of the group in a pinned `golang`
image:

1. downloading a module through the **proxy** member — this fetches from
   `proxy.golang.org`, so it needs network egress and **skips** when the module
   cannot be reached;
2. publishing a module (`.info`/`.mod`/`.zip`) to the **hosted** member and
   resolving it through the private group with a Bearer token, then retaining
   one complete version through cleanup — fully hermetic, no upstream.

Run it with `bats examples/go/test.bats` (needs `bats` and Docker).
