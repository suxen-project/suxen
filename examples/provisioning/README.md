# Provisioning (config-as-code with suxenctl apply)

`suxenctl apply -f FILE` reconciles a declarative document like `kubectl apply`:
it is idempotent, previews with `--dry-run`, deletes omitted owned resources with
`--prune`, and marks what it creates as **managed** — protected from ad-hoc API
edits unless forced. Secrets are never in the document; they are pulled at apply
time from the environment or a file via `secretRef`.

## Server side

```sh
examples/provisioning/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml) (one repository, `alpha`) — the baseline the commands
below build on.

## Client / operator side

Preview, then apply. A re-apply of the same document changes nothing:

```sh
suxenctl apply -f desired.yaml --dry-run   # reports created/updated/deleted, applies nothing
suxenctl apply -f desired.yaml             # idempotent: unchanged on a repeat
```

Prune deletes resources that provisioning previously created but the document no
longer lists (bootstrap repositories `raw`/`oci` and roles `anonymous`/
`administrator` are retained, even for `resources: []`):

```sh
suxenctl apply -f smaller.yaml --prune
```

On updates, omit a top-level `spec` field to preserve it. Supplying a field
replaces its entire value: an updated `criteria` list removes old predicates,
and an updated `groupRoles` map removes omitted group grants. Supply the full
desired list or map when changing one entry. A prune result of `skipped` means
ownership changed after planning and the resource was left in place.

Managed resources reject ad-hoc edits, so config-as-code stays the source of
truth:

```sh
suxenctl repo delete alpha
# -> 409 managed_resource: "alpha" is managed by declarative provisioning
curl -X DELETE -H "Authorization: Bearer $SUXEN_TOKEN" \
  "$SUXEN_URL/api/v1/repositories/alpha?force=true"     # override deliberately
```

Provide secrets without committing them. `secretRef` reads a value at apply time
from an environment variable or an absolute file path:

```yaml
- kind: user
  name: ci
  spec:
    roles: [writer]
    secretRef:
      env: CI_PASSWORD      # or: file: /run/secrets/ci-password
```

```sh
export CI_PASSWORD=...
suxenctl apply -f users.yaml
```

## Test

[`test.bats`](test.bats) asserts: `--dry-run` creates nothing; re-apply is
`unchanged`; `--prune` removes an omitted owned repo while keeping the bootstrap
ones; a managed repo returns `409 managed_resource` and `?force=true` overrides;
`secretRef` creates users from both an env var and a file; numeric policy values
survive apply and partial updates exactly; and invalid YAML numbers reject the entire
document before creating resources. Hermetic
(suxenctl + a pinned `curl` image); needs only Docker. Run it with `bats
examples/provisioning/test.bats`.
