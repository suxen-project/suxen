# S3 blob store (MinIO)

By default the examples keep blobs in memory. Production suxen stores them in an
S3-compatible object store. This example runs a **dedicated support stack** —
one MinIO server — and points a natively-run suxen at it, so uploaded bytes land
in an S3 bucket instead of local disk.

suxen still runs via the same launcher; only the object store is containerized.
S3 is a "shared" blob store, so suxen requires strong bootstrap credentials
(token ≥ 24 chars, password ≥ 12) — the launcher's example defaults already
satisfy this.

## Server side

```sh
examples/s3/start.sh
```

This:

1. builds the checksum-pinned MinIO and `mc` tools from
   [`test/support/minio/Dockerfile`](../../test/support/minio/Dockerfile), then brings up
   [`compose.yaml`](compose.yaml) (MinIO on `127.0.0.1:9000`, console
   on `:9001`) and waits for it to be healthy;
2. creates the `suxen-example` bucket;
3. runs the launcher with
   `SUXEN_BLOBSTORE=s3://suxen-example/blobs?region=us-east-1&endpoint=http://127.0.0.1:9000&pathStyle=true`
   and `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` for MinIO, applying
   [`repo.yaml`](repo.yaml).

Ctrl-C stops suxen and tears the stack down (`compose down -v`).

The `endpoint` and `pathStyle=true` query parameters point the S3 driver at
MinIO; against real AWS S3 you omit `endpoint`, drop `pathStyle`, and supply the
region and credentials for your account.

## Client side

In a separate client shell, export `SUXEN_URL` and `SUXEN_TOKEN` using the URL
and admin token printed by `start.sh`.

Nothing special — clients use suxen exactly as in the other examples; the S3
backing is invisible to them:

```sh
printf 'hello from S3\n' > hello.txt
curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file hello.txt \
  "$SUXEN_URL/repository/files/demo/hello.txt"
curl "$SUXEN_URL/repository/files/demo/hello.txt"
```

Inspect the object store directly (the MinIO console is at
`http://127.0.0.1:9001`, `minioadmin`/`minioadmin`):

```sh
docker compose -f examples/s3/compose.yaml run --rm --no-deps mc \
  ls --recursive local/suxen-example
```

## Test

[`test.bats`](test.bats) uploads a raw asset, checks the round-trip, and
confirms an object exists in the bucket. It needs Docker (for MinIO and the
`mc`/`curl` clients). The test builds the MinIO tools before its readiness
timer starts. Run it with `bats examples/s3/test.bats`.
