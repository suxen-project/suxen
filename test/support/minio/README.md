# Test-only MinIO images

This Dockerfile builds the MinIO server and `mc` client from immutable Go
module versions corresponding to their official 2025 releases. It replaces
anonymous pulls of MinIO container images, which may require registry
credentials. The build fetches source and dependencies through the public Go
module proxy and verifies them with the Go checksum database. It does not
depend on the upstream binary archive.

From the repository root:

```sh
docker build --target minio -t suxen-test-minio test/support/minio
docker build --target mc -t suxen-test-mc test/support/minio
docker run --rm suxen-test-minio --version
docker run --rm suxen-test-mc --version
```

The server target includes `curl` for the Compose readiness probe. The `mc`
target uses `mc` as its entry point, so Compose can pass `alias`, `mb`, and
other commands directly.
