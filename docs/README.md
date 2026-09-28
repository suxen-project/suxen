# suxen documentation

The root [README](../README.md) is the product overview and quick start. This index routes
the detailed guides by audience.

## Get started

- [Getting started](getting-started.md): run the server and make a first push and pull.
- [Concepts](concepts.md): content addressing, repository formats and types, assets and
  attributes, the policy model, and identity.

## Use suxen

- [Consumer guide](guides/consumers.md): connect to an existing suxen from clients and CI.
- [Repositories](guides/repositories.md): hosted, proxy, and group repositories.
- [Blob stores](guides/blobstores.md): named storage backends and their safety guards.
- [Authorization](guides/authorization.md): privilege grammar, least-privilege roles,
  anonymous access, and token scoping.
- [OIDC](guides/oidc.md): external identity providers and browser login.
- [Classification and cleanup](guides/classification-cleanup.md): labels, typed cleanup
  predicates, and garbage collection.
- [Webhooks and gates](guides/webhooks-gates.md): event delivery and quarantine gates.
- [Provenance and signing](guides/provenance.md): trust policies and signature
  verification.

## Reference

- [API guide](reference/api.md): authentication, privileges, pagination, errors, and
  data-plane examples.
- [`suxenctl` reference](reference/cli.md): every bundled CLI command.
- [Configuration](reference/configuration.md): the single-source environment-variable
  table.
- [Metrics](reference/metrics.md): exposed series, label cardinality, and the leader
  signal.
- [Compatibility and support policy](reference/compatibility.md): v1 support window,
  API compatibility, storage migrations, and chart/image pairing.

## Operate suxen

- [Install with Docker Compose or from source](operations/install-compose.md).
- [Install with Helm](operations/install-helm.md).
- [High availability](operations/high-availability.md): the stateless PostgreSQL/S3
  topology and a three-replica values file.
- [Capacity and availability](operations/capacity.md): measured one/three-replica
  baseline, upload and timeout bounds, memory sizing, and proxy outage behavior.
- [Operations guide](operations/operations.md): ingress, credential rotation, backups,
  restore, and upgrades.
- [Provisioning](operations/provisioning.md): desired-state documents, secrets, ownership,
  dry-run, and pruning.
- [Troubleshooting](operations/troubleshooting.md): HTTP failures, readiness, storage,
  schedulers, authentication, proxies, webhooks, and metrics triage.
- [Helm chart guide](../charts/suxen/README.md): values, Secrets, topology, and upgrades.

## Extend and contribute

- [Architecture](contributing/architecture.md): design principles, domain model, runtime
  shape, storage and metadata seams, the policy model, and extensibility.
- [Extension guide](contributing/extending.md): public SPIs, compiled-in plugins, build
  tags, custom binaries, and conformance suites.
- [Administration UI](contributing/administration-ui.md): routes, forms, identity, and
  privilege-aware actions.
- [Testing](contributing/testing.md): source checks and the black-box interoperability
  tier.
- [Release procedure](contributing/releasing.md): signed tags, artifact checks,
  publication, and verification.
- [Contributing guide](../CONTRIBUTING.md): development setup, test tiers, compatibility
  surfaces, and pull-request expectations.
- [Changelog](../CHANGELOG.md): unreleased and released user-visible changes.
