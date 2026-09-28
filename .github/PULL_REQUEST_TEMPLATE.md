<!--
Keep the change focused; unrelated changes belong in a separate pull request.
See CONTRIBUTING.md for the full contribution guidelines.
-->

## Outcome

<!-- The user or operator outcome this change delivers. -->

## Implications

<!-- Compatibility, security, and migration implications. Call out any change to a
compatibility-sensitive surface: HTTP paths, JSON fields, privilege strings,
provisioning kinds, CLI nouns, database migrations, or plugin interfaces. Write "none"
if there are none. -->

## Validation

<!-- The commands you ran and that passed, e.g. `make check`, focused integration
tests, or `SUXEN_E2E_FULL=1 make test-interop` when a data-plane protocol, auth,
ingress, blob storage, or clustered flow changed. -->

## Follow-ups

<!-- Any intentional follow-up work or unsupported case. Write "none" if there are
none. -->

## Checklist

- [ ] `make check` passes
- [ ] OpenAPI document and contract tests updated for any control-plane API change
- [ ] Forward-only migration added for any persisted schema change
- [ ] Operator, API, chart, CLI, or extension docs updated alongside the behavior
- [ ] User-visible changes recorded in the current release entry or `Unreleased` in `CHANGELOG.md`
