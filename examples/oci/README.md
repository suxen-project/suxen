# OCI registry (images and artifacts)

A hosted OCI registry for container images, Helm charts, and arbitrary OCI
artifacts. suxen serves the OCI `/v2` API at the **registry root** of its listen
host, routing it to the repository named `oci`, so any OCI client
(`docker`, `oras`, `crane`, `helm`) addresses it as `<suxen-host>/<name>:<tag>` —
no `/repository/` path.

## Server side

```sh
examples/oci/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and provisions
[`repo.yaml`](repo.yaml) at startup, so the `dockerhub` listener binds port
`5001`. That port must be free before starting the example. Changing a bound
port later requires a server restart. The launcher prints the URL and admin
token; Ctrl-C stops it.

## Client side

Pushing needs write on `oci`; the example development profile grants anonymous pulls. For a quick test, use the
admin credentials; for CI, attach the `image-publisher` role from `repo.yaml` to
a scoped user (see the language examples for the `secretRef` user pattern).

Docker. The daemon treats a loopback registry (`127.0.0.1`/`localhost`) as
insecure automatically; for any other plain-HTTP host, add it to the daemon's
`insecure-registries` (or front suxen with TLS):

```sh
docker login suxen.example.com          # scoped user or admin
docker tag myapp:latest suxen.example.com/myapp:latest
docker push suxen.example.com/myapp:latest
docker pull suxen.example.com/myapp:latest
```

oras, for non-image artifacts:

```sh
oras push suxen.example.com/notes:v1 notes.txt:text/plain
oras pull suxen.example.com/notes:v1
```

Helm charts are OCI artifacts. `helm registry login` needs `--insecure` to accept
a plain-HTTP registry; `push`/`pull` take `--plain-http`:

```sh
helm registry login suxen.example.com -u admin -p "$PW" --insecure
helm package ./mychart                            # -> mychart-0.1.0.tgz
helm push mychart-0.1.0.tgz oci://suxen.example.com --plain-http
helm pull oci://suxen.example.com/mychart --version 0.1.0 --plain-http
```

### Cache Docker Hub (proxy on a bound port)

The `dockerhub` repository is a pull-through cache of Docker Hub. OCI is the only
format that takes `endpoints`, and a second OCI repository needs its own `/v2`
root — so `dockerhub` is bound to port `5001` and used as a standalone registry:

```sh
docker pull 127.0.0.1:5001/library/alpine:latest   # add 127.0.0.1:5001 as an insecure registry
oras manifest fetch --plain-http 127.0.0.1:5001/library/hello-world:latest
```

suxen handles Docker Hub's Bearer-token challenge and caches manifests (revalidated
on the proxy TTL) and blobs (immutable). The primary port's `/v2` root always
serves the repository named `oci`; a bound **host** routes by `Host` header, a
bound **port** opens an extra listener serving only the `/v2` API — the control
plane and UI stay on the primary port.

## Test

[`test.bats`](test.bats) round-trips the registry with each documented client:

1. **oras** (pinned image) pushes an artifact to the registry root and pulls it
   back, comparing the bytes — hermetic;
2. **docker** builds a tiny `FROM scratch` image, pushes it, and pulls it back —
   hermetic. This one uses the **host** `docker` (a client needs the host daemon;
   there is none inside a pinned image); auth is isolated in a per-test
   `DOCKER_CONFIG`, and a loopback registry is insecure by default, so no daemon
   config is needed;
3. **helm** (pinned image) packages a starter chart, pushes it, and pulls it back
   — hermetic;
4. **dockerhub proxy**: fetch a public image manifest through the proxy on port
   5001 — needs network egress and **skips** when Docker Hub is unreachable.

Run it with `bats examples/oci/test.bats` (needs `bats` and Docker).

## Protecting published tags

The hosted repository in `repo.yaml` sets `allowOverwrite: false`. Publishing a
new tag succeeds; moving an existing tag to a different manifest returns a conflict.
An identical manifest retry remains allowed. Set `allowOverwrite: true` to allow
moving tags. Digest-addressed content always retains its identity. The ORAS test
checks that a rejected tag replacement leaves the original artifact downloadable.
