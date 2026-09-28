# Provenance with Sigstore cosign

suxen verifies signatures server-side against a per-repository **trust policy**.
This example uses [Sigstore cosign](https://docs.sigstore.dev/) — the tool teams
actually sign with — to sign a release artifact, and a `verify-on-push` policy
that accepts only artifacts signed by a trusted key.

The [`trust-policy/`](../trust-policy/) example shows the same feature with plain
`openssl` keys; this one shows the cosign workflow specifically.

## Server side

```sh
examples/cosign/start.sh
```

Applies [`repo.yaml`](repo.yaml): a hosted raw repository `releases`. The trust
policy is set at runtime, because the operator generates the cosign key pair and
the public key cannot be committed.

## Client / operator side

Generate a cosign key pair and register the public key in a `verify-on-push`
policy (an unsigned upload is then refused):

```sh
COSIGN_PASSWORD= cosign generate-key-pair --output-key-prefix cosign
# Fold the PEM into a JSON string and set the policy.
suxenctl trust-policy set releases policy.json   # {"mode":"verify-on-push","publicKeys":["<cosign.pub PEM>"]}
```

suxen verifies the signature over the artifact's **digest string**
(`sha256:<hex>`) when no explicit payload is supplied. Sign that string with
`cosign sign-blob` and send the base64 signature in the `X-Suxen-Signature`
header:

```sh
printf 'sha256:%s' "$(sha256sum release.tar.gz | cut -d' ' -f1)" >digest.txt
COSIGN_PASSWORD= cosign sign-blob --key cosign.key \
  --output-signature release.sig --tlog-upload=false --yes digest.txt

curl -H "Authorization: Bearer $SUXEN_TOKEN" \
  -H "X-Suxen-Signature: $(tr -d '\n' <release.sig)" \
  -X PUT --upload-file release.tar.gz \
  "$SUXEN_URL/repository/releases/release.tar.gz"        # 201, then served
```

cosign's ECDSA signature is base64, which is exactly what `X-Suxen-Signature`
expects. (The `suxenctl raw put --signature` flag base64-encodes a *raw* binary
signature, so use the header for cosign, not that flag.)

ECDSA, RSA (PKCS#1 v1.5 / PSS), and Ed25519 signatures are accepted. Keyless
signing against a Fulcio CA works through `certificateAuthorities` +
`allowedIdentities`; see [docs/guides/provenance.md](../../docs/guides/provenance.md).

## Scope

This example covers the **blob-signing** path (a detached signature over the
digest), which suxen verifies for raw and OCI uploads. cosign's **OCI 1.1
referrer** signing (`cosign sign <image>` writing a signature manifest) is not
exercised here: under `verify-on-pull` the signer must first pull the
not-yet-released manifest, which the gate withholds — a known interop gap, so it
is left out rather than documented as working.

## Test

[`test.bats`](test.bats) generates a cosign key pair, sets a `verify-on-push`
policy, and asserts: an unsigned upload is refused (403); a correctly signed
artifact is accepted (201) and served (200); and a signature from an untrusted
key is refused (403). Hermetic — keys are local and signing uses
`--tlog-upload=false` (no transparency log). Needs Docker (cosign + curl images).
Run it with `bats examples/cosign/test.bats`.
