# Install with Docker Compose or from source

This page covers a single-node installation — the default topology of one process backed
by SQLite and a filesystem blob store. For a multi-replica, fault-tolerant deployment see
[high availability](high-availability.md) and [install with Helm](install-helm.md).

## Docker Compose

```sh
export SUXEN_BOOTSTRAP_USER=admin
export SUXEN_BOOTSTRAP_PASSWORD='replace-with-a-long-password'
export SUXEN_BOOTSTRAP_TOKEN='replace-with-a-random-token-at-least-24-characters'
docker compose up --build -d
docker compose logs suxen
```

This starts a persistent local installation. Data is stored on a mounted volume and
survives restarts.

The runtime image contains the server but not `suxenctl`. Use the REST examples in the
project README for a Compose-only first upload, or run `make build` when you want the host
CLI. Pull an OCI source image before tagging it; for example,
`docker pull alpine:3.24`.

## From source

Requirements: Go 1.26.8 or newer.

```sh
make build
./bin/suxen serve
```

The source binary listens on `127.0.0.1:8080`. The runtime image listens on `:8080`
inside its container network, while Compose publishes it only on host loopback. State is
stored under `./data`, and the first start creates two hosted
repositories on first start: `raw` for arbitrary files and `oci` for images and OCI
artifacts.

## First-start credentials

If bootstrap credentials are not configured, secure random credentials are logged once
when the database is first created. Set them explicitly in unattended installs with
`SUXEN_BOOTSTRAP_USER`, `SUXEN_BOOTSTRAP_PASSWORD`, and `SUXEN_BOOTSTRAP_TOKEN`. All
configuration variables are documented in the
[configuration reference](../reference/configuration.md).

## Storage sizing

Uploads are staged before becoming visible. Filesystem deployments can briefly need
roughly twice an upload's size on the data volume. Size the data volume above
`SUXEN_MAX_UPLOAD_BYTES` (default 10 GiB).

Run suxen behind a TLS reverse proxy outside trusted development networks. The
[operations guide](operations.md) covers ingress, credential rotation, backup and restore,
and upgrades.
