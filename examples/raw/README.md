# Raw files

A hosted `raw` repository stores arbitrary files addressed by path — build
artifacts, tarballs, release assets. Writes require the `repository:files:write`
privilege. The example development profile grants anonymous reads explicitly.

## Server side

```sh
examples/raw/start.sh
```

This builds and runs a local suxen (SQLite metadata + an in-memory blob store,
no Docker) and applies [`repo.yaml`](repo.yaml), which declares a replaceable hosted raw
repository named `files` and a protected one named `releases`. The command prints the server URL and an admin token,
and stays in the foreground; press Ctrl-C to stop. `repo.yaml` is applied like
`kubectl apply` — declarative and idempotent, so re-running is a no-op:

```sh
suxenctl apply -f examples/raw/repo.yaml   # against an already-running server
```

## Client side

The examples reach the server as `$SUXEN_URL` with the admin token in
`$SUXEN_TOKEN` (`http://127.0.0.1:8080` and the printed token by default). Any
HTTP client works; here is `curl`.

```sh
# Authenticated upload.
echo "hello from suxen" > hello.txt
curl -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file hello.txt "$SUXEN_URL/repository/files/demo/hello.txt"

# Anonymous download.
curl -fsS "$SUXEN_URL/repository/files/demo/hello.txt"
```

An anonymous upload is refused with `401`/`403`, because the example's development
profile grants the anonymous role only `repository:*:read`.

## Replacing files

`files` sets `allowOverwrite: true`: upload different bytes to the same path to
replace it. `releases` sets `allowOverwrite: false`: uploading identical bytes
again succeeds, while a different payload at that path returns 409 and leaves
the original available. New paths remain writable.

```sh
curl -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file hello.txt "$SUXEN_URL/repository/releases/demo/hello.txt"
# Retry the same upload: succeeds.
curl -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file hello.txt "$SUXEN_URL/repository/releases/demo/hello.txt"
echo "changed" > replacement.txt
# Returns 409.
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file replacement.txt "$SUXEN_URL/repository/releases/demo/hello.txt"
```

## Test

[`test.bats`](test.bats) runs the same workflow with a binary payload (in a
pinned `curlimages/curl` image over the host network), compares the downloaded
bytes exactly, asserts the anonymous-write refusal, and checks both replacement
policies including identical retries. Run it with
`bats examples/raw/test.bats` (needs `bats` and Docker).
