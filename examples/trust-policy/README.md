# Trust policy (provenance verification)

A trust policy verifies provenance for raw assets and OCI manifests against a set
of public keys. In `verify-on-push` mode an upload is rejected (**403**,
`provenance_rejected`) unless it carries a signature, over the asset's digest,
made by one of the policy's keys. `verify-on-pull` gates downloads the same way;
`audit` verifies and records the result but never blocks.

This example uses plain public-key signatures (ECDSA/RSA/Ed25519 over the
`sha256:<hex>` digest string) — no cosign or external service. The
certificate-identity (Fulcio) and OCI cosign-referrer paths need the
support-service tier.

## Server side

```sh
examples/trust-policy/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml), which puts a `verify-on-push` policy on the `raw`
repository with a **throwaway example public key whose private half is not
shipped** — so every upload to `raw` is rejected until you replace the key with
your own. It also creates a `signed` repository whose policy is left unset for
you to manage.

## Client / operator side

Generate a keypair (the private key stays with you and is never committed):

```sh
openssl ecparam -name prime256v1 -genkey -noout -out priv.pem
openssl ec -in priv.pem -pubout -out pub.pem
```

Register the public key. Either put it in `repo.yaml` under `publicKeys` and
re-apply, or set it on a repository whose policy you manage out-of-band:

```sh
suxenctl trust-policy set signed policy.json   # {"mode":"verify-on-push","publicKeys":["<pub.pem>"]}
```

Sign the asset's digest string and push. The signed payload is `sha256:<hex>`,
where `<hex>` is the sha256 of the file's bytes:

```sh
hex=$(sha256sum art.txt | cut -d' ' -f1)
printf '%s' "sha256:$hex" | openssl dgst -sha256 -sign priv.pem > sig.der
suxenctl raw put --signature sig.der signed prov/art.txt art.txt
```

`suxenctl raw put --signature` base64-encodes the binary signature into the
`X-Suxen-Signature` header. An unsigned upload to the same repository is
rejected with 403.

## Test

[`test.bats`](test.bats) asserts an unsigned upload to `raw` is rejected
(hermetic), then generates a key, registers it on `signed`, signs an asset, and
asserts the signed upload is served — the full operator workflow. The signed
path needs `openssl` and **skips** without it. Run it with `bats
examples/trust-policy/test.bats`.
