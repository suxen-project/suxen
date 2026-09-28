# Security Policy

suxen is an artifact registry that holds credentials, proxies upstream registries, and
enforces signature and provenance policies. Its security posture — and the boundaries of
what it verifies — are treated as first-class documentation. This file explains how to
report a vulnerability and states the known boundaries so operators can reason about
their own threat model.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately**. Do not open a public issue for a
security report.

- Use GitHub's private vulnerability reporting: on the repository's **Security** tab,
  choose **Report a vulnerability**. This opens a private advisory visible only to you
  and the maintainers.

Include, as far as you can:

- the affected version or commit;
- a description of the issue and its impact;
- reproduction steps or a proof of concept;
- any suggested remediation.

### What to expect

- **Acknowledgement:** within 5 business days.
- **Assessment:** an initial severity and validity assessment within 10 business days.
- **Fix and disclosure:** we aim to release a fix and publish an advisory as soon as a
  remediation is available, coordinating a disclosure date with the reporter. Reporters
  who wish to be credited will be named in the advisory.

These response targets are maintained on a best-effort basis rather than as contractual
guarantees.

## Supported versions

The latest v1 minor line receives security fixes. There is no long-term-support branch;
operators should track the latest release. The release and compatibility policy is documented in
[`docs/reference/compatibility.md`](docs/reference/compatibility.md).

## Known security boundaries

These are deliberate, documented limits of the current implementation — not
vulnerabilities. They are listed so operators can compensate in their own environment.

- **Signature and provenance verification is not a full Sigstore verifier.** suxen
  verifies the signature, the certificate validity window, the Fulcio issuer, and the
  SAN identity of cosign/DSSE material. It does **not** yet validate Rekor transparency-log
  inclusion, signed certificate timestamps, RFC 3161 timestamps, or Sigstore bundle
  material. When enforcement must hold without a transparency-log verifier, configure
  trust policies with pinned public keys or currently valid certificate authorities.
- **Egress protection is a deny-list, not an allow-list.** The outbound HTTP client used
  for proxy upstreams and webhooks pins DNS results, strips redirects, and blocks
  known-internal address ranges, with per-host and per-CIDR exceptions. It defends
  against common SSRF pivots but does not, by default, restrict egress to an explicit
  allow-list of destinations.
- **Anonymous access is controlled by a normal role.** The built-in `anonymous` role is
  empty on a fresh installation. Operators may explicitly grant named or wildcard
  repository reads for public artifacts. Unauthenticated `GET /v2/` returns 401 with
  Bearer (when `SUXEN_OIDC_STATE_SECRET` is set) and Basic so Docker can mint a public
  pull token or send stored credentials.
- **Extra OCI listen ports are registry-only.** Bindings from `endpoints.ports` serve
  `/v2`, `/healthz`, and `/readyz`. They do not expose `/api/v1` or the administration UI.
  Prefer hostname routing plus Ingress in multi-replica deployments.
- **Build version is disclosed only by the dedicated endpoint.** `/version` exposes the
  running build for operability and client compatibility checks. Unrelated responses do
  not carry a version header.

## Security controls in place

For completeness, these protections are implemented and enabled by default:

- Control-plane `/api/v1` paths check privilege from the URL before the route table, so
  unknown control-plane paths return `401`/`403` rather than revealing whether the route
  exists. Unknown top-level paths return `404`.
- CSRF protection guards cookie- and OIDC-session-authenticated mutations. Token- and
  basic-authenticated API clients do not rely on ambient cookie credentials.
- Failed password and token verification is throttled to mitigate brute-force attempts.
- Token scopes intersect correctly, including for administrators; secrets (authorization
  headers, OIDC tokens, webhook secrets, resolved provisioning secrets, signed upstream
  URLs) are never logged.
- Blob storage is content-addressed and digest-verified on write. Reads serve the
  digest-keyed path; they do not re-hash filesystem objects.
