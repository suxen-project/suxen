# OIDC authentication

External identities can authenticate with a Bearer ID token issued by a configured OIDC
provider. suxen discovers the provider's JWKS, validates the signature, issuer, audience,
and expiration, then maps claim values to local roles. How claims map to privileges is
covered in the [authorization guide](authorization.md); this guide covers provider
configuration and interactive browser login.

## Configuring a provider

Configure a provider with the administration API:

```sh
curl -H "Authorization: Bearer $SUXEN_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{
    "name": "corporate",
    "issuer": "https://identity.example/realms/engineering",
    "clientId": "suxen",
    "clientSecret": "replace-me",
    "scopes": ["openid", "profile", "email", "groups"],
    "groupsClaim": "groups",
    "defaultRoles": [],
    "groupRoles": {"release-engineering": ["raw-publisher"]}
  }' \
  http://localhost:8080/api/v1/oidc-providers
```

The referenced roles must already exist. Client secrets are never returned by the API;
omitting `clientSecret` from an update keeps the existing value.

## Password grant for non-interactive clients

`docker login` and CI service accounts (for example a `ci-runner` bot user) send an HTTP Basic
username and password and cannot follow a browser redirect. To let these credentials
resolve against an OIDC provider, enable the OAuth2 Resource Owner Password Credentials
grant on that provider with `allowPasswordGrant`:

```sh
curl -H "Authorization: Bearer $SUXEN_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{
    "name": "internal",
    "issuer": "https://identity.example/realms/service-accounts",
    "clientId": "suxen",
    "clientSecret": "replace-me",
    "allowPasswordGrant": true
  }' \
  http://localhost:8080/api/v1/oidc-providers
```

When a Basic credential matches no local user, suxen forwards the username and password
to the token endpoint of each provider that has `allowPasswordGrant` enabled, in name
order, until one issues a token; the returned ID token is then validated exactly like a
Bearer login. `docker login -u <service-account> -p <password> <registry>` then works.

Enable this only for an internal provider you control. The password leaves suxen and is
sent to the IdP, the grant bypasses MFA, and it is deprecated in OAuth2 — keep
`allowPasswordGrant` off (the default) for any remote provider. The provider's client
must permit direct access grants (in Keycloak, enable "Direct Access Grants" on the
client). Because credentials are tried against every enabled provider, prefer keeping a
single provider enabled to avoid forwarding one provider's password to another.

## Browser login

For browser login, set `SUXEN_OIDC_STATE_SECRET` to a shared random value of at least 32
characters (the same secret also signs OCI Distribution Bearer tokens) and open:

```text
/auth/oidc/corporate/login?redirect=/
```

The administration UI offers the same flow without typing a URL: while the state secret
is configured, `GET /api/v1/whoami` lists every provider in `loginProviders`, and the UI
shows a "Sign in with `<provider>`" button for each one to anonymous visitors. The
button returns to the page that was open once the callback completes.

This uses Authorization Code with an S256 PKCE challenge, a signed state cookie, and an
OIDC nonce. The callback stores the validated ID token in an HTTP-only session cookie, so
each server instance can validate it independently without shared session state. Use
`POST /auth/oidc/corporate/logout` to clear that cookie.

The session lasts as long as the ID token's own expiry. `GET /api/v1/whoami` returns that
expiry as `expiresAt` (Unix seconds) and the issuing provider as `sessionProvider`, so the
administration UI renews the session before it lapses: when a signed-in user navigates
while the session is close to expiry, the UI performs a `prompt=none` re-authentication
(`/auth/oidc/corporate/login?prompt=none`). If the provider still has a live session it
returns a fresh token with no interaction; if it answers `login_required` the still-valid
session keeps working and the UI shows a visible "Sign in with `<provider>`" button only
once the session has actually expired. A `prompt=none` attempt is made at most once per
session expiry, so a provider that cannot re-authenticate silently never causes a redirect
loop.

Set `SUXEN_PUBLIC_URL` when the externally visible origin differs from the request origin
seen by suxen. Mutating requests authenticated by this cookie must carry a matching
browser `Origin` header; this same-origin check protects the UI session from cross-site
request forgery. Basic and Bearer authentication are not subject to the cookie-specific
check.
