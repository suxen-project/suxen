# Getting started

This guide takes you from nothing to a running suxen with a first Raw and OCI push and
pull. If you are connecting to a suxen someone else operates, start with the
[consumers guide](guides/consumers.md) instead.

## Run the server

The fastest local start is Docker Compose or a source build; both are covered in
[install with Docker Compose or from source](operations/install-compose.md). In short:

```sh
make build
./bin/suxen serve
```

The server listens on `127.0.0.1:8080`, stores state under `./data`, and creates two hosted
repositories on first start: `raw` and `oci`. If bootstrap credentials are not configured,
secure random credentials are logged once when the database is first created — capture
them. All settings are in the [configuration reference](reference/configuration.md).

The standard build also serves an embedded administration UI at `http://localhost:8080/`.
Paste the logged bootstrap API token into the header's token sign-in form. The token stays
in browser session storage and is cleared when that tab signs out or closes.
It provides repository creation and asset management, policy and trust configuration,
users/roles/tokens, OIDC providers, webhooks, task history, and garbage collection. The
CLI and REST API remain available for complete automation.

## First Raw artifact

```sh
export SUXEN_URL=http://localhost:8080
export SUXEN_TOKEN=replace-with-the-logged-bootstrap-token
printf 'hello from suxen\n' > project-v1.txt

./bin/suxenctl raw put raw source/project-v1.txt ./project-v1.txt
./bin/suxenctl raw get raw source/project-v1.txt ./downloaded.txt
```

The HTTP API is deliberately simple:

```sh
curl -u admin:replace-with-the-logged-bootstrap-password \
  --upload-file project-v1.txt \
  http://localhost:8080/repository/raw/source/project-v1.txt

curl -H "Authorization: Bearer $SUXEN_TOKEN" --output downloaded.txt \
  http://localhost:8080/repository/raw/source/project-v1.txt
```

Fresh installations grant the `anonymous` role no privileges. Reads, uploads, and deletes
therefore require a local password or Bearer token until an operator explicitly makes a
repository public.

## First OCI image

The default `oci` repository is exposed at the standard Distribution API root, so Docker,
Podman, Buildah, ORAS, and Helm OCI clients can use it directly:

```sh
docker login localhost:8080
docker pull alpine:3.24
docker tag alpine:3.24 localhost:8080/example/alpine:3.24
docker push localhost:8080/example/alpine:3.24
docker pull localhost:8080/example/alpine:3.24
```

The standard OCI root challenges Docker during the Distribution handshake. Log in before
both pushes and pulls on a fresh installation. An operator can grant selected public reads
through the `anonymous` role; Docker also needs `SUXEN_OIDC_STATE_SECRET` for the anonymous
Bearer-token handshake.
For clients that can configure a path prefix, the same repository is available at
`/repository/oci/v2/`. Additional OCI repositories can be bound to other hostnames or
listen ports; see [Repositories](guides/repositories.md#oci-registry-roots).

## Next steps

- [Concepts](concepts.md) — repository types, content addressing, and the policy model.
- [Repositories](guides/repositories.md) and [blob stores](guides/blobstores.md) —
  creating and administering storage.
- [Authorization](guides/authorization.md) — roles, tokens, and least-privilege access.
- [Install with Helm](operations/install-helm.md) and
  [high availability](operations/high-availability.md) — production topologies.
