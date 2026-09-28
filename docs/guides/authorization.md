# Authorization guide

suxen authorizes a request in three steps: users receive roles, roles grant privileges,
and API-token scopes can narrow the resulting privilege
set. Administrator users bypass role lookup, but a scoped administrator token is still
restricted by its scopes.

Use `suxenctl whoami` before debugging an authorization failure. It reports the current
authentication kind, roles, and effective privileges without exposing credentials.
`GET /api/v1/privileges` returns the server's current privilege catalog. With
`SUXEN_URL` and `SUXEN_TOKEN` set as in the [API guide](../reference/api.md), run
`curl -H "Authorization: Bearer $SUXEN_TOKEN" "$SUXEN_URL/api/v1/privileges"`
using a token that has `admin:privileges:read`.

## Privilege grammar

Repository privileges have this shape:

```text
repository:<repository>:<read|write|delete|annotate|manage>
```

- `read` covers pulls, downloads, browsing, asset metadata, repository policy reads,
  `GET /api/v1/repositories/{name}`, and listing repositories the principal can read.
- `write` covers Raw and OCI uploads.
- `delete` covers artifact deletion and manual repository cleanup.
- `annotate` covers asset attributes and provenance verification.
- `manage` covers classification rules, download gates, and trust policies. Give
  this to policy operators; asset scanners do not need it to submit annotations.

Global control-plane privileges use:

```text
admin:<resource>:<read|write|run>
```

The resource vocabulary is `provision`, `blob-stores`, `download-gate-defaults`,
`trust-policy-defaults`, `classification-defaults`, `repositories`, `stats`,
`users`, `roles`, `privileges`, `oidc-providers`, `cleanup-policies`, `webhooks`,
`tasks`, `gc`, and `verify`. Compiled-in plugin identifiers appear in
`GET /api/v1/privileges` as `plugin-{pluginID}` resources and use
`admin:plugin-{pluginID}:read|write`.
This keeps a plugin named like a core resource from sharing its grants.
`POST /api/v1/gc` and `POST /api/v1/verify` require `admin:gc:run` and
`admin:verify:run`, respectively. Task history reads use `admin:tasks:read`.
Other mutations use `write`.

A `*` matches one segment. A final `*` also matches the remaining suffix, so `admin:*`
grants all administrative operations, while `admin:*:read` grants reads across all
administrative resources. Prefer the narrowest complete privilege set. The bare `*`
grant is equivalent to unrestricted access and should be reserved for the built-in
administrator role.

## Common role designs

An automation publisher for one repository usually needs only:

```yaml
- kind: role
  name: release-publisher
  spec:
    privileges:
      - repository:releases:read
      - repository:releases:write
```

Read is included because OCI clients commonly perform existence checks during a push.
Add `repository:releases:annotate` only when the same principal writes attributes or
submits provenance verification material. Give `repository:releases:manage` separately
to operators who change classification or artifact acceptance policies. Keep deletion
in a separate maintenance role:

```yaml
- kind: role
  name: release-maintainer
  spec:
    privileges:
      - repository:releases:delete
```

Roles are flat: a role is a named set of privileges and never includes another role.
Compose privileges at the assignment boundary instead — a principal who both publishes
and deletes is assigned both `release-publisher` and `release-maintainer`:

```yaml
- kind: user
  name: release-bot
  spec:
    roles: [release-publisher, release-maintainer]
```

A control-plane auditor can read global configuration and metrics without mutating it.
Inspecting repository configuration uses the same `repository:<name>:read` grant as
artifact GET, so add named or wildcard repository reads when the auditor should see
those documents:

```yaml
- kind: role
  name: control-plane-auditor
  spec:
    privileges:
      - admin:*:read
      - admin:stats:read
      - repository:*:read
```

The explicit stats grant is retained for clarity even though `admin:*:read` already
matches it. A Prometheus token should normally carry only `admin:stats:read`.

## Anonymous, local, and OIDC identities

Unauthenticated requests use the built-in `anonymous` role. It starts with no privileges,
so a fresh installation is private. To publish one repository, reconcile that role with a
named read grant:

```yaml
- kind: role
  name: anonymous
  spec:
    privileges: [repository:public-releases:read]
```

The broader `repository:*:read` grant makes every repository publicly readable for
artifact GET/HEAD. Anonymous `docker pull` additionally needs
`SUXEN_OIDC_STATE_SECRET` so the Distribution token endpoint can sign a pull token.
Unauthenticated `GET /v2/` still returns 401 (Bearer plus Basic) so Docker can mint that
token or send stored credentials. Do not use an ingress authentication layer as the only
restriction: keep Suxen's anonymous role aligned with the intended policy.

Local users have explicitly assigned roles. OIDC users receive `defaultRoles` plus
roles selected from the configured group claim. Keep OIDC group-to-role mappings small
and review them alongside identity-provider group ownership. Unknown external groups do
not create privileges.

## Deleting a role

Deleting a role removes its direct user-role assignments in the same operation, and
deleting a user removes that user's assignments. A role that is still named by an OIDC
provider's `defaultRoles` or group mapping cannot be deleted: the request is rejected
with `409 role_in_use_by_provider`. Remove the provider mappings first, then delete the
role. A role that declarative provisioning manages is rejected with `409
managed_resource` unless the request sets `force=true`, which transfers ownership away
from provisioning before deleting.

## Token scoping and rotation

Token scopes are an intersection, not an additional grant. A token cannot exceed the
user's effective roles. Give CI a scoped token even when its user is more privileged:

```sh
suxenctl user token \
  --name release-ci \
  --scopes repository:releases:read,repository:releases:write \
  build-agent
```

Token values are returned once. Store them in a Secret manager, create and deploy a
replacement before revoking the old token, and use `suxenctl whoami` with the new token
to verify its effective privileges.

OCI clients exchange their primary credential for a bearer token valid for five
minutes. Local bearers remain bound to the account that authenticated: deleting
that account invalidates them, and recreating its username never restores their
access. Current administrator status and role privileges are checked on each
request, within the token's original repository and action scopes.

Changing a password or revoking a primary API token prevents new exchanges with
that credential. An already issued OCI bearer can remain valid until its expiry;
it cannot renew itself. External OIDC bearers retain their captured role names for
the same bounded lifetime, with current role privileges checked on use.

Upgrading through migration 0014 preserves local passwords, API tokens, and role
assignments. Local OCI bearers issued before the upgrade lack the account identity
binding and are rejected; clients must repeat the token exchange using their
primary credential.

## Review checklist

For each principal, verify:

1. every granted repository name still exists and has the intended visibility;
2. wildcard grants are necessary rather than convenient;
3. publisher roles do not include deletion unless the workflow requires it;
4. automation tokens are scoped below their user's maximum access;
5. the anonymous role matches the deployment's public/private stance;
6. OIDC group ownership is reviewed by the identity-provider administrator;
7. `admin:provision:write`, `admin:users:write`, and `admin:roles:write` are limited to
   trusted operators because they can materially change future access.

The running OpenAPI document includes `x-suxen-required-privilege` on every
control-plane operation. Treat it as the authoritative operation-to-privilege mapping.
