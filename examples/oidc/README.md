# OIDC federated login (identity provider → roles)

suxen can delegate authentication to an external OpenID Connect provider. Users
have no local password in suxen; they authenticate against the IdP, and suxen
maps the verified identity to roles. This example federates to a [Dex](https://dexidp.io)
IdP running in a dedicated compose stack.

The **browser redirect** flow (`/auth/oidc/<provider>/login`) is for humans in a
web UI. Non-interactive clients — CI jobs, `docker login`, `curl` — use the
**OAuth2 password grant** instead: they send HTTP Basic credentials, and suxen
exchanges them with the IdP behind the scenes. This example uses the password
grant, because it is what a headless client and a test can drive.

## Server side

```sh
examples/oidc/start.sh
```

`start.sh` brings up Dex (see [`compose.yaml`](compose.yaml) and
[`dex.yaml`](dex.yaml)), waits for its discovery endpoint, then runs a local
suxen and applies [`repo.yaml`](repo.yaml). Because the IdP lives on the loopback
address, `start.sh` sets `SUXEN_OUTBOUND_ALLOWED_HOSTS=127.0.0.1` (suxen's
outbound client denies loopback by default) and exports the client secret that
`repo.yaml` references.

## Provider configuration

```yaml
- kind: oidcProvider
  name: dex
  spec:
    issuer: http://127.0.0.1:5556/dex          # must match the IdP's discovery/iss
    clientId: suxen
    secretRef: { env: SUXEN_OIDC_CLIENT_SECRET } # resolved at apply time, never inlined
    scopes: [openid, email]
    allowPasswordGrant: true                    # enable the non-interactive grant
    defaultRoles: [oidc-writer]                 # roles every IdP user receives
```

`allowPasswordGrant` opts this provider into the password grant. `defaultRoles`
are granted to everyone who authenticates through it. The client secret is
resolved from an env var (or an absolute file path) when the document is applied,
so it is never committed — the same `secretRef` mechanism used for blob stores
and provisioned users.

## Client side

The IdP knows a user `alice@example.com` (password `alice-password`) that suxen
has no local account for. Sending those credentials as HTTP Basic makes suxen run
the password grant, verify the returned `id_token`, and grant `oidc-writer`:

```sh
# Push to a repository only writers may write to — authenticated via the IdP.
curl -u alice@example.com:alice-password \
  -X PUT --data 'hello' "$SUXEN_URL/repository/app/alice.txt"   # 201

# Anonymous still cannot write.
curl -X PUT --data 'hello' "$SUXEN_URL/repository/app/anon.txt" # 401

# whoami shows the federated identity and its effective privileges.
curl -u alice@example.com:alice-password "$SUXEN_URL/api/v1/whoami"
# {"authenticationKind":"oidc-password","username":"alice@example.com",
#  "roles":["oidc-writer"],"effectivePrivileges":[...,"repository:app:write"]}
```

`docker login`, `npm`, `twine`, and `cargo` all send HTTP Basic the same way, so
the same IdP credentials authenticate any of them against suxen.

## Per-group roles

Beyond `defaultRoles`, suxen can map an IdP **group** to roles with `groupsClaim`
+ `groupRoles` (e.g. group `platform` → role `admin`). That needs an IdP
connector that emits a `groups` claim (LDAP, GitHub, Google, …); Dex's static
password database does not, so this example demonstrates `defaultRoles` only. The
full field set is in
[docs/operations/provisioning.md](../../docs/operations/provisioning.md).

## Test

[`test.bats`](test.bats) asserts that the IdP user writes via the password grant
(201) and reads it back; a wrong IdP password is refused (401); anonymous cannot
write (401) but keeps its read grant (200); and `whoami` reports
`authenticationKind: oidc-password` with the `oidc-writer` role and the
`repository:app:write` privilege. Needs Docker (for Dex and the curl client).
Run it with `bats examples/oidc/test.bats`.
