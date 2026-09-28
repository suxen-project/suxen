# RBAC (users, roles, scoped tokens)

suxen's access model is **privilege → role → subject**. A role bundles
privileges like `repository:<name>:<read|write|delete|annotate>`; users hold
roles; API tokens carry **scopes** that *intersect* with their user's
privileges (a token can never grant more than its user has). The loopback-only example
profile grants `repository:*:read` to the otherwise-empty built-in `anonymous` role.

## Server side

```sh
examples/rbac/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml): two hosted repositories (`app`, `logs`) and a `writer`
role that can write to both.

## Client / operator side

Create a user with the role, then mint a token **scoped narrower** than the user
(here: `app` only, even though the user can also write `logs`):

```sh
suxenctl user create --password "$CI_PASSWORD" --roles writer ci
suxenctl user token --name ci-token --scopes repository:app:write ci
# -> {"token":"<secret>", ...}
```

Use the token, and check what it can do:

```sh
export SUXEN_TOKEN=<secret>
suxenctl whoami                       # effectivePrivileges: [repository:app:write]

curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file a.txt \
  "$SUXEN_URL/repository/app/demo/a.txt"     # 201 — in scope

curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file a.txt \
  "$SUXEN_URL/repository/logs/demo/a.txt"    # 403 — user can, token cannot
```

The `logs` write is refused even though the *user* holds `repository:logs:write`:
the token's scope is the ceiling. Revoke a token with
`suxenctl user revoke ci <token-id>` (list them with `suxenctl user tokens ci`).

Anonymous callers can read but not write:

```sh
curl "$SUXEN_URL/repository/app/demo/a.txt"                 # 200
curl --upload-file a.txt "$SUXEN_URL/repository/app/x.txt"  # 401
```

## Test

[`test.bats`](test.bats) creates the scoped user + token, and asserts:
in-scope write → 201, out-of-scope write → 403, `whoami` shows the intersected
privileges, development-profile anonymous read → 200, anonymous write → 401. Hermetic (suxenctl +
a pinned `curl` image); needs only Docker. Run it with `bats
examples/rbac/test.bats`.
