# suxen

suxen is a small, self-hosted artifact repository for Raw files, OCI images, and
language-ecosystem packages — Maven, Go modules, Cargo crates, npm, PyPI, and Git
snapshots. Raw, OCI, Maven, Go modules, Cargo, npm, and PyPI support hosted, proxy, and
group repositories; Git snapshots are proxy-only. It stores content by SHA-256 digest,
deduplicates identical uploads, and the default installation is one static Go binary
backed by SQLite and a filesystem blob store; multi-replica installations use PostgreSQL
and an S3-compatible object store without changing the binary.

It is a deliberately stripped-down, permissively licensed homage to Sonatype Nexus
Repository — hence the reversed name. suxen is an independent project, not affiliated with
or endorsed by Sonatype; Nexus is a trademark of Sonatype.

The compatibility surface remains prerelease until `v1.0.0` is published. The
[changelog](CHANGELOG.md) summarizes the `v1.0.0-rc.1` candidate, and the
[compatibility policy](docs/reference/compatibility.md) identifies the intended stable
contract.

## Scope

suxen's scope is the life of an artifact: storing it, serving it, caching it from
upstreams, verifying it, and applying retention and access policy to it. The one server
covers four jobs along that life, and this set is the intended boundary rather than a
starting point:

- **Test** — one static binary with a memory or filesystem blob store makes a throwaway
  registry an integration test can start, fill, and discard.
- **Cache** — proxy repositories serve upstreams (Docker Hub, Maven Central,
  proxy.golang.org, crates.io, npm, PyPI, git hosts) through one cache. Warm immutable
  artifacts avoid upstream traffic; expired mutable metadata still requires a successful
  upstream revalidation. npm, Cargo, and PyPI groups also consult package metadata to select
  the member supplying an artifact.
- **Quarantine** — download gates withhold a newly stored or proxied artifact until a
  scanner, signature, or provenance check clears it.
- **Produce** — a multi-replica tier over PostgreSQL and object storage serves as the
  authoritative store for what you publish. A repository's `allowOverwrite` setting
  controls replacement of published artifacts and OCI tags.

Everything outside that boundary is a deliberate non-goal, so the default deployment stays
small and behavior stays predictable. suxen is **not a git hosting platform** — no
repository hosting, issues, pull requests, or CI — and it is neither a build/CI system nor
a general developer platform. The `git` format is a read-only pull-through proxy that
serves upstream commits as self-contained snapshots; it does not host or mirror writable
repositories, and pushes are refused.

## Quick start

From a clone, choose first-start credentials and start a persistent local instance:

```sh
export SUXEN_BOOTSTRAP_USER=admin
export SUXEN_BOOTSTRAP_PASSWORD='replace-with-a-long-password'
export SUXEN_BOOTSTRAP_TOKEN='replace-with-a-random-token-at-least-24-characters'
docker compose up --build -d
docker compose logs suxen
```

Or build and run from source (Go 1.26.8 or newer):

```sh
make build
./bin/suxen serve
```

The server listens on `127.0.0.1:8080`, stores state under `./data`, and creates a `raw` and an
`oci` hosted repository on first start. Bootstrap credentials are logged once when the
database is first created. The standard build also serves an administration UI at
`http://localhost:8080/`; paste the logged bootstrap token into its token sign-in form.
The initial `anonymous` role has no privileges, so artifact access is private until an
operator grants public reads explicitly.

Then push and pull your first artifacts. This Compose path uses `curl`,
so it does not assume a host `suxenctl` binary, and pulls the source image before tagging
it:

```sh
export SUXEN_URL=http://localhost:8080
export SUXEN_TOKEN='replace-with-a-random-token-at-least-24-characters'

# Raw file
printf 'hello from suxen\n' > app-v1.txt
curl -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
  --upload-file app-v1.txt \
  "$SUXEN_URL/repository/raw/source/app-v1.txt"
curl -fsS -H "Authorization: Bearer $SUXEN_TOKEN" \
  -o downloaded.txt \
  "$SUXEN_URL/repository/raw/source/app-v1.txt"

# OCI image (the root challenges Docker, so log in first)
printf '%s' "$SUXEN_BOOTSTRAP_PASSWORD" | \
  docker login -u "$SUXEN_BOOTSTRAP_USER" --password-stdin localhost:8080
docker pull alpine:3.24
docker tag alpine:3.24 localhost:8080/example/alpine:latest
docker push localhost:8080/example/alpine:latest
docker pull localhost:8080/example/alpine:latest
```

The [getting-started guide](docs/getting-started.md) walks through this in full.

## Features

- **Raw and OCI** repositories in hosted, proxy, and group types. OCI implements
  Distribution v2 with the OCI 1.1 referrers API. Package formats are supplied by
  compile-time plugins: Maven and Go modules, Cargo, npm, and PyPI (hosted, proxy,
  group), and git snapshots (proxy).
- **Content-addressed storage** with deduplication and digest verification, on filesystem,
  S3-compatible, or GCS blob stores.
- **Stateless replicas.** An application tier over PostgreSQL and shared object storage
  gives process fault tolerance and spreads reads and request preprocessing; writes, GC,
  and migration to one
  physical blob store serialize under a shared lease. Single-node SQLite is a complete
  install.
- **Identity and access in the free product:** OIDC SSO, scoped and revocable API tokens,
  and role/privilege RBAC with a configurable anonymous role.
- **Supply-chain features:** Cosign signatures and digest-bound DSSE provenance checks
  with per-repository trust policies, and attribute-driven quarantine download gates.
- **Config-as-code:** declarative provisioning with dry-run, pruning, and ownership
  enforcement, plus durable HMAC-signed webhooks with delivery history.
- **Retention:** typed cleanup-policy predicates, classification labels, and blob garbage
  collection on an independent schedule.
- **Operations:** Prometheus metrics, health and readiness probes, structured logs, an
  embedded administration UI, and a `noui` build variant — with no usage limits.

## Documentation

Start at the [documentation index](docs/README.md), which routes by audience:

- **Use it:** [getting started](docs/getting-started.md) ·
  [concepts](docs/concepts.md) · [consumer guide](docs/guides/consumers.md) ·
  [CLI reference](docs/reference/cli.md) · [API guide](docs/reference/api.md)
- **Operate it:** [install with Helm](docs/operations/install-helm.md) ·
  [high availability](docs/operations/high-availability.md) ·
  [capacity and availability](docs/operations/capacity.md) ·
  [operations](docs/operations/operations.md) ·
  [configuration](docs/reference/configuration.md) ·
  [troubleshooting](docs/operations/troubleshooting.md)
- **Extend it:** [architecture](docs/contributing/architecture.md) ·
  [extension guide](docs/contributing/extending.md)

## Interoperability

The checked-in release workflow has separate evidence tiers. Single-node executable
examples exercise Go, Cargo, npm, pip/Twine, Maven, Git, ORAS, Docker, Helm, and operator
workflows. A black-box three-replica suite over PostgreSQL and MinIO exercises Docker,
ORAS, Crane, Cosign, Maven, Gradle, curl, and Prometheus where cluster behavior matters.
Browser checks cover anonymous overview rendering, artifact-origin isolation, native
confirmation behavior, authenticated repository CRUD, cookie-session CSRF protection,
and OIDC callback and logout flows against a mock identity provider. See
[the release test matrix](docs/contributing/release-test-matrix.md),
[the testing guide](docs/contributing/testing.md), and
[test/e2e/README.md](test/e2e/README.md).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).
Report vulnerabilities privately per [SECURITY.md](SECURITY.md). User-visible changes are
tracked in [CHANGELOG.md](CHANGELOG.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
