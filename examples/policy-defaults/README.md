# Instance-wide policy defaults (inheritGlobal)

A policy — a download gate, a classification rule set, or a trust policy — can be
declared **once for the whole instance** instead of per repository. Each
repository then inherits that default, composes its own policy with it, or opts
out. This example uses the **download gate**; classification and trust policy
work the same way (a `classification` or `trustPolicy` named `default`).

## Server side

```sh
examples/policy-defaults/start.sh
```

Applies [`repo.yaml`](repo.yaml): an instance-wide default download gate that
withholds any asset until it carries `scan.status=passed`, plus three raw
repositories that relate to it differently.

## The three relationships

The default gate is the singleton `downloadGate` named `default`:

```yaml
- kind: downloadGate
  name: default
  spec:
    criteria: [{path: "scan.status", op: "=", value: "passed"}]
    enabled: true
```

- **`strict`** has no gate of its own, so it is gated by the default alone: an
  asset is withheld until `scan.status=passed`.
- **`reviewed`** has its own gate (`review.status=approved`) and inherits the
  default, so **both** criteria must pass (logical AND) — scan *and* review.
- **`open`** opts out. A repository gate with `inheritGlobal=false` uses only its
  own criteria, and an empty list gates nothing.

`inheritGlobal` is a **runtime-only** flag — a provisioned repository gate always
inherits (`inheritGlobal=true`), so `open` starts with no gate and is opted out
through the API:

```sh
echo '{"criteria":[],"enabled":true,"inheritGlobal":false}' >opt-out.json
suxenctl download-gate set open opt-out.json
```

`enabled` follows the repository's own gate when it has one, otherwise the
default's. Clear the default with `suxenctl download-gate defaults delete`; with
none configured, each repository behaves exactly as its own gate specifies.

## Test

[`test.bats`](test.bats) asserts: an asset in `strict` is withheld (403) until
`scan.status=passed` (200) — the default applies to a repository with no gate; an
asset in `reviewed` needs both `review.status=approved` and the inherited
`scan.status=passed` (403 with only one, 200 with both) — the AND-composition;
and `open` is withheld by the default (403) until a runtime `inheritGlobal=false`
gate opts it out (200). Hermetic: suxenctl (admin) + a pinned curl image; needs
only Docker. Run it with `bats examples/policy-defaults/test.bats`.
