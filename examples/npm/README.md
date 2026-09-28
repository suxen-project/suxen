# npm packages

Three repositories that work together: `npm-proxy` caches the public registry
(`registry.npmjs.org`), `npm-hosted` accepts internal `npm publish`, and `npm`
(a group over both) is one registry URL serving internal and cached public
packages.

## Server side

```sh
examples/npm/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml). Prints the URL and admin token; Ctrl-C to stop.

## Client side

### Publish

Publishing needs a token with `repository:npm-hosted:write`. Attach the
`npm-publisher` role from `repo.yaml` to a scoped user by applying a second
document (its password comes from the environment, so nothing is committed):

```yaml
apiVersion: suxen.io/v1
resources:
  - kind: user
    name: ci
    spec:
      admin: false
      roles: [npm-publisher]
      secretRef:
        env: NPM_CI_PASSWORD
```

Point npm at the registry and set the token in `.npmrc`. npm sends the
`_authToken` as a bearer token, which suxen accepts as an API token — issue one
for the scoped user (or use the admin token for a quick test):

```ini
; .npmrc
registry=https://suxen.example.com/repository/npm-hosted/
//suxen.example.com/repository/npm-hosted/:_authToken=<token>
```

```sh
npm publish
```

The `_authToken` key includes the repository path and the trailing slash.
`dist-tags.latest` tracks the highest published release; custom publish tags
(`npm publish --tag`) are not preserved.
When versions have equal SemVer precedence, the lexicographically greatest full
version string wins, keeping the generated tag stable across reads.

### Install

The example launcher grants anonymous reads for manual exploration. On a normal private
repository, configure an `_authToken` for the group path just as for the hosted path.
Point at the **group** to get both internal and cached public packages from one URL:

```sh
npm install --registry https://suxen.example.com/repository/npm/ <pkg>
```

or set `registry=` in `.npmrc` and `npm install` normally. Public packages
requested through the group (or `npm-proxy` directly) are fetched from
`registry.npmjs.org` and cached.
The proxy caches full package metadata even when an install client asks for the
smaller npm representation. Later `npm view` and group reads retain the full
description and readme; older abbreviated cache entries refresh on their next
metadata read.
Historical public names such as `JSONStream` also resolve through the proxy
and group, with tarball URLs rewritten to the suxen repository. Hosted
publication still requires modern lowercase package names.

## Test

[`test.bats`](test.bats) covers both halves in a pinned `node` image:

1. publish a package with `npm publish` and resolve it back from the private repository (`npm view`,
   `npm pack`) — hermetic, no upstream;
2. publish distinct packages to the hosted member and a local source behind a
   proxy, then resolve both through one group URL — hermetic;
3. `npm pack` a public package through the **proxy** — needs network egress and
   **skips** when the registry is unreachable.

Run it with `bats examples/npm/test.bats` (needs `bats` and Docker).
