# Contributing to Suxen

Thank you for helping improve Suxen. Changes should preserve the project's core
properties: a small default deployment, predictable artifact behavior, explicit
operator controls, and a stable documented API.

## Before starting

Open an issue or discussion before a large feature, a new wire-protocol format, a
database compatibility change, or a public SPI change. Small fixes, tests, and
documentation corrections can go directly to a pull request.

Proposals are weighed against the project's scope — the life of an artifact: storing,
serving, caching, verifying, and applying policy to it (see [Scope](README.md#scope)).
Features that turn suxen into a source-code host, a build/CI system, or a general
developer platform are out of scope by design, not yet-to-be-built.

Public plugin interfaces have additional compatibility constraints. Read
[the extension guide](docs/contributing/extending.md) before changing code under `spi/` or adding a
compiled-in plugin.

## Development environment

The core source build requires Go 1.26.8 or newer. The complete validation suite also
uses Node.js for the dependency-free administration UI tests. Docker, Compose, Bats,
and the pinned external clients are needed only for the black-box interoperability
tier.

Build the server and CLI:

```sh
make build
```

Run the same source checks as the main CI job:

```sh
make check
```

Run the real-client interoperability tier when changing a data-plane protocol,
authentication, ingress behavior, blob storage, or clustered request flow:

```sh
SUXEN_E2E_FULL=1 make test-interop
```

The installer and scenario requirements are documented in
[`test/e2e/README.md`](test/e2e/README.md).

## Change guidelines

- Keep implementation code readable and conventionally formatted. Do not compress
  control flow or declarations to reduce line count.
- Add comments to exported declarations and to interfaces whose behavioral contract
  is not obvious from their method signatures.
- Treat HTTP paths, JSON fields, privilege strings, provisioning kinds, CLI nouns,
  database migrations, and plugin interfaces as compatibility-sensitive surfaces.
- Update `internal/server/openapi.json` and its contract tests with every control-plane
  API change.
- Add a forward-only migration for persisted schema changes. Never rewrite a released
  migration.
- Keep tests deterministic. Time-based tests need enough headroom for race-detector and
  loaded CI execution.
- Never log credentials, authorization headers, OIDC tokens, webhook secrets, resolved
  provisioning secrets, or signed upstream URLs.
- Update operator, API, chart, CLI, or extension documentation in the same pull request
  as the behavior it describes.

## Pull requests

Use a focused feature branch and target `main` in the canonical
[`suxen-project/suxen`](https://github.com/suxen-project/suxen) repository. Keep unrelated
changes out of the pull request. A stacked pull request is welcome when one
independently reviewable change depends on another; state the dependency and use the
preceding feature branch as its base.

The pull request description should include:

1. the user or operator outcome;
2. compatibility, security, and migration implications;
3. the validation commands that passed;
4. any intentional follow-up or unsupported case.

Before requesting review, run `make check` and the relevant focused integration tests.
CI also validates the scale-out topology, real-client interoperability, UI-capable
container image, and Helm chart. The project publishes only the standard image with the
embedded administration UI; `noui` remains a locally built variant.

## Documentation

Start at the [documentation index](docs/README.md). Keep examples copyable, use stable
resource names, and distinguish default behavior from optional configuration. New
configuration values must be documented in both the relevant operator guide and Helm
values when the chart exposes them.

Release and compatibility expectations are defined in
[the compatibility policy](docs/reference/compatibility.md). User-visible changes belong
in the pending release entry, or under `Unreleased` after GA, in
[CHANGELOG.md](CHANGELOG.md).
