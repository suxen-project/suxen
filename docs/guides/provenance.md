# Provenance and signing

suxen stores signatures and attestations natively and verifies them server-side against a
per-repository trust policy. The verification boundary — what is and is not checked — is
documented in [SECURITY.md](../../SECURITY.md).

## Trust policies

A repository trust policy accepts pinned public keys, X.509 certificate authorities,
optional certificate identities, and denied SHA-256 public-key fingerprints. The denylist
always wins. Enforcement is one of:

- `audit`: verify and record a verdict without blocking transfer;
- `verify-on-pull`: accept uploads, but serve an artifact only after it passes;
- `verify-on-push`: require a valid signature while uploading to a hosted repository.

Trust policies belong to hosted or proxy repositories. Group repositories reject all
trust-policy modes: downloads through a group enforce the policy of the member that
supplies the artifact, including that member's inherited instance default. Configure
each member that needs enforcement. A repository with a policy cannot be converted to
a group until its policy is removed.

`verify-on-push` is rejected for proxy repositories and for Cargo, npm, and PyPI
hosted repositories because their native publish envelopes cannot carry Suxen's detached
signature headers. Use `verify-on-pull` or `audit` for those formats.

Save a policy such as the following as `trust-policy.json`. PEM values contain their
literal newlines escaped for JSON.

```json
{
  "mode": "verify-on-pull",
  "publicKeys": [
    "-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----\n"
  ],
  "certificateAuthorities": [],
  "allowedIdentities": [],
  "deniedFingerprints": []
}
```

```sh
./bin/suxenctl trust-policy set raw trust-policy.json
./bin/suxenctl trust-policy get raw
```

### Instance-wide default

A single instance-wide default policy can back every hosted or proxy repository:

```sh
./bin/suxenctl trust-policy defaults set trust-policy.json
```

A repository inherits the default only while it has no policy of its own; defining a
repository policy overrides the default entirely (there is no partial merge of modes or
material). Clear the default with `trust-policy defaults delete`; with no default and no
repository policy, provenance-bearing assets are recorded but not enforced. There is no
per-repository "off" switch — a repository that must ignore an inherited default defines
its own policy instead. Instance defaults support `audit` and `verify-on-pull` only;
`verify-on-push` must be set directly on a compatible hosted repository.

## Signed Raw uploads

For a signed Raw upload, sign the exact lowercase `sha256:<hex>` digest string. The CLI
base64-encodes the detached signature and optional PEM certificate into the upload
headers:

```sh
artifact_digest="sha256:$(sha256sum release.tar.gz | cut -d ' ' -f 1)"
printf '%s' "$artifact_digest" |
  openssl dgst -sha256 -sign signing-key.pem -out release.tar.gz.sig

./bin/suxenctl raw put \
  --signature release.tar.gz.sig \
  raw releases/release.tar.gz release.tar.gz
```

ECDSA ASN.1, RSA PKCS#1 v1.5/PSS with SHA-256, and Ed25519 signatures are accepted.
Supplying `--certificate signing-certificate.pem` verifies a currently valid code-signing
certificate against the configured authorities. `allowedIdentities` entries match the
certificate's exact email or URI SAN and its Fulcio OIDC issuer:

```json
{
  "issuer": "https://token.actions.githubusercontent.com",
  "subject": "https://github.com/example/project/.github/workflows/release.yml@refs/heads/main"
}
```

## Cosign, DSSE, and OCI referrers

Cosign simple-signing payloads and in-toto/SLSA DSSE envelopes can be submitted for an
existing asset with `suxenctl verify REPOSITORY ASSET_ID REQUEST.json`. `signature` and
`payload` are base64 strings; a DSSE request uses the standard `payloadType`, `payload`,
and `signatures` envelope fields. Successful and failed results are stored under the
reserved `provenance` attribute namespace, which external callers cannot overwrite.
Changing a trust policy invalidates older verdicts until the artifact is verified again.

OCI 1.1 signatures and attestations remain ordinary OCI artifacts discoverable through the
referrers API. When a referrer layer contains a cosign signature annotation or a DSSE
envelope, suxen verifies it against the policy and releases every tag alias for the
referenced subject digest. A `verify-on-push` OCI workflow may instead sign the manifest
digest through the `X-Suxen-Signature` upload header.

## Verification boundary

suxen's verifier is not a full Sigstore verifier: it checks the certificate chain and
code-signing usage, validity, Fulcio issuer, and SAN identity, but does not validate
transparency-log inclusion or timestamp material. [SECURITY.md](../../SECURITY.md) states
the exact boundary. Where enforcement must hold without a transparency-log verifier,
configure trust policies with pinned keys or currently valid certificates.
