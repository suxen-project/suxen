# Blob stores

Blob stores are named resources that hold the content-addressed bytes behind a
repository's assets. Each repository selects one with `blobStore`. First startup
creates the `default` store from `SUXEN_BLOBSTORE` (`fs://<data>/blobs` when unset).

## Creating a blob store

Create additional stores from a JSON document whose configuration is referenced from the
server environment or an absolute mounted file:

```json
{
  "name": "archive",
  "driver": "s3",
  "configurationRef": {"env": "SUXEN_BLOBSTORE_ARCHIVE"}
}
```

```sh
export SUXEN_BLOBSTORE_ARCHIVE='s3://archive-bucket/suxen?region=eu-west-1'
./bin/suxenctl blob-store create archive-blobstore.json
./bin/suxenctl blob-store list
./bin/suxenctl repo create --format raw --blob-store archive archived-raw
```

`configurationRef` accepts exactly one `env` name or absolute `file` path. suxen resolves
the value only while opening the compiled-in driver and never returns or persists that
resolved value through the administration API; only a one-way SHA-256 identity is stored
for collision and rotation detection. Protect the referenced environment variable or file
as a secret when a custom driver puts credentials in its configuration.

The built-in S3 driver rejects URL user information and obtains credentials from the
standard AWS SDK credential chain; its URL contains only bucket, prefix, region,
endpoint, and path-style settings. Available blob drivers depend on the build: `fs` and
`s3` are built into the core and `gcs` ships in the default build. See the
[extension guide](../contributing/extending.md) for the plugin set and the blob SPI.

## Safety guards

Physical backend identities are hashed and unique, so two names cannot accidentally point
at the same filesystem root or S3 bucket/prefix. A store's driver, configuration reference,
and physical destination are fixed for its lifetime, even while unused. Operational
`attributes`, including upload-session limits, can be updated on named stores and on
`default`. The `default` store cannot be deleted or drained.

Lowering upload-session quotas below existing staged usage blocks further writes,
but clients can still cancel uploads and garbage collection can reap stale sessions.
Cleanup also remains available after the server-wide upload size limit is lowered.

Credentials may rotate behind the same reference only when the physical destination stays
the same. Restart processes that read a referenced value at startup; see
[credential rotation](../operations/operations.md#credentials-and-secret-rotation). Reapplying
an unchanged resource does not move its destination. To move bytes, create a new store and
[drain](../operations/operations.md#draining-a-blob-store) the old one. An unused named
store may instead be deleted and created afresh. Repositories containing assets cannot
change blob stores in place.

Configuration values for the built-in drivers and cluster requirements are documented in
the [configuration reference](../reference/configuration.md).
