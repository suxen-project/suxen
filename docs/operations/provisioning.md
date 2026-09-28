# Declarative provisioning

Suxen reconciles control-plane resources from one YAML or JSON document. The
same engine serves startup provisioning, the administration API, and
`suxenctl apply`.

```yaml
apiVersion: suxen.io/v1
resources:
  - kind: blobStore
    name: constrained
    spec:
      driver: fs
      configurationRef:
        env: SUXEN_BLOBSTORE_CONSTRAINED
      attributes:
        uploadSessions:
          staleAfter: 15m
          maxStagedBytes: 5368709120
          maxPrincipalStagedBytes: 1073741824
          maxPrincipalSessions: 2

  - kind: repository
    name: constrained-raw
    spec:
      format: raw
      type: hosted
      blobStore: constrained

  - kind: role
    name: publisher
    spec:
      description: Publish Raw artifacts
      privileges:
        - repository:constrained-raw:write

  - kind: user
    name: automation
    spec:
      admin: false
      roles: [publisher]
      secretRef:
        env: AUTOMATION_PASSWORD

  - kind: cleanupPolicy
    name: constrained-disk
    spec:
      repositories: [constrained-raw]
      criteria:
        - path: sys.blobStore
          op: "="
          value: constrained
        - path: sys.lastAccessed
          op: before
          value: 7d
      keepLast: 2
      action: delete
      enabled: true
```

Set `SUXEN_BLOBSTORE_CONSTRAINED` to an `fs://` or `s3://` driver configuration before
applying this example. The cleanup policy is therefore self-contained: it owns the
named store and repository that its `sys.blobStore` predicate selects.

`attributes.uploadSessions` applies retention and aggregate staged-upload limits to
the physical blob store. This is the appropriate place for aggressive cleanup on a
space-constrained store: `staleAfter` is per-store and sets how long an inactive session
survives before the garbage-collection job removes it, while the byte and session budgets
bound both total staging and each authenticated principal. All limits must be positive;
omitted fields use the server defaults.

When `uploadSessions` or one of its fields is omitted, Suxen uses a six-hour inactivity
window, four concurrent sessions per principal, and byte budgets equal to
`SUXEN_MAX_UPLOAD_BYTES` for both the complete blob store and each principal. These
finite defaults preserve one maximum-sized upload while bounding abandoned staging.
Explicit attributes override them; the example above intentionally uses a 15-minute,
two-session policy for constrained storage.

Supported kinds are `blobStore`, `repository`, `role`, `user`,
`oidcProvider`, `cleanupPolicy`, `classification`, `trustPolicy`,
`downloadGate`, and named `webhook`. A `classification`, `trustPolicy`, or
`downloadGate` resource is named for its repository — except the reserved name
`default`, which addresses that kind's instance-wide policy default.
Specs are partial on existing resources: omitted top-level `spec` fields retain
their stored value. A supplied field replaces its complete stored value, including
maps, arrays, and nested objects; omitted entries inside a supplied map or object
are removed. To revoke one grant in `groupRoles`, supply the complete desired
`groupRoles` map. To remove one predicate, supply the complete desired `criteria`
array. An empty map or array clears that field where the resource schema permits it.
Numeric predicate values retain their exact value through parsing and partial updates,
including integers larger than JavaScript's safe integer range. YAML values that cannot
be represented in JSON, such as `.nan` and `.inf`, are rejected before reconciliation;
an invalid value never turns a supplied specification into an empty one.
Hosted repository specs accept `allowOverwrite: true` or `false`. Omission on creation
uses the format default; omission on an existing repository preserves its value.
See [replacing existing assets](../guides/repositories.md#replacing-existing-assets)
for defaults, identical retries, and mutable index behavior.

The built-in `raw` and `oci` repositories and the `anonymous` and `administrator` roles
are creation defaults: Suxen creates them when absent and protects their managed records
from pruning, but does not overwrite existing state during an unrelated apply or restart.
Explicitly declaring one of these resources reconciles it normally. To keep one
imperatively controlled after using `force=true`, remove it from the desired document;
a later apply that still declares it restores its desired state and ownership.

The instance-wide default of a `classification`, `trustPolicy`, or `downloadGate`
is the singleton named `default`, which repositories inherit; its spec is the same
shape as the per-repository resource. Because `default` addresses the singleton, it
is a reserved repository name, and a document may declare each kind's `default` at
most once (a second is rejected as a duplicate resource). Provisioning a default at
deploy time makes it read-only at runtime (see "Ownership and imperative changes");
a default set only through the runtime API stays editable there.

```yaml
  - kind: downloadGate
    name: default
    spec:
      criteria:
        - {path: scan.status, op: "=", value: passed}
      enabled: true
```

References are ordered automatically. Blob stores precede repositories;
repository members precede groups; roles precede users and OIDC mappings; and
repositories precede policies, gates, classification, and webhooks. Reference
cycles reject the complete document before any mutation.

A `trustPolicy` resource may target a hosted or proxy repository, or the `default`
singleton. Group policies are rejected during preflight, including when the group is
created in the same document. Downloads through groups enforce the supplying member's
policy and its inherited instance default. Repository format and type are immutable:
to introduce a group, create it under a new name with hosted or proxy members. Keep
the members' policies if those policies should govern downloads through the group.

## Apply and prune

```sh
suxenctl apply -f desired-state.yaml --dry-run
suxenctl apply -f desired-state.yaml
suxenctl apply -f desired-state.yaml --prune
```

Apply reports `created`, `updated`, `unchanged`, `deleted`, `skipped`, or `failed`
for each resource. `skipped` means a planned prune left the resource in place after
its ownership was transferred away. It is idempotent. Pruning is disabled unless
explicitly requested and only removes resources previously owned by declarative
provisioning. An empty `resources: []` document with `--prune` removes eligible
owned resources; built-in `raw` and `oci` repositories and `anonymous` and
`administrator` roles are protected from pruning.

The REST equivalent is
`POST /api/v1/provision?dryRun=false&prune=false`, protected by
`admin:provision:write`. HTTP requests must use the canonical
`apiVersion`/`resources` envelope with `application/json` or
`application/yaml`. Local files accepted by startup and `suxenctl` may also be
a top-level resource list, one resource, or JSONL; they are normalized to the
canonical envelope before transmission.
Each YAML input must contain one document; `---` streams with additional
documents are rejected. Use a `resources` list or a provisioning directory to
combine resources.

## Secrets

Users (`password`), OIDC providers (`clientSecret`), proxy repositories
(`upstream`), and webhooks (`secret`) accept either an inline value or exactly
one reference:

```yaml
secretRef:
  env: SECRET_ENVIRONMENT_VARIABLE
```

```yaml
secretRef:
  file: /absolute/mounted/secret
```

References and inline values are mutually exclusive, and empty resolutions fail
validation without changing state. `suxenctl` requires an authentication token
before it reads the provisioning file or resolves references, resolves references
locally, and requires HTTPS for remote servers (plain HTTP is accepted only on loopback).
Startup and direct API requests resolve references in the server environment.
The provisioning ownership ledger persists only a salted, non-reversible secret
fingerprint. The reconciled resource still stores the secret according to that
resource's normal semantics: local passwords and API tokens are one-way hashed, while
proxy upstream credentials, OIDC client secrets, and webhook HMAC secrets must remain
recoverable and are therefore stored in the metadata database. Encrypt and restrict
database storage and backups accordingly. Normal API/store secret changes invalidate
the provisioning fingerprint, so reapplying the same desired reference detects and
restores out-of-band drift. Supplying the existing value preserves the fingerprint and
ownership. An omitted secret never clears or overwrites an existing value.

## Ownership and imperative changes

Provisioned resources report `"managed": true` from their normal control-plane
endpoints. List the complete ownership ledger with:

```text
GET /api/v1/provision
```

This inventory requires `admin:provision:read` and exposes only resource kind, name,
and update time. Comparison hashes and secret fingerprints remain internal.

Normal `PUT`, `DELETE`, and managed user-role updates return `409 managed_resource`
instead of creating drift. To intentionally transfer one resource to imperative
management, repeat the mutation with `?force=true`. Suxen applies the mutation and
removes its current ownership record. For lasting imperative control, also remove the
resource from the desired document. A later apply that still declares it restores
the document's desired state and ownership.
API tokens attached to a managed user remain independently mutable because tokens are
not part of the provisioning document.

## Startup and Helm

Set `SUXEN_PROVISION=file:///absolute/document.yaml` after mounting a file, or use a
`file://` URL to an absolute directory of `.yaml`, `.yml`, `.json`, and `.jsonl`
files. Directory files are combined in lexical order and duplicate resource identities
are rejected.
Startup reconciliation runs after database migration and bootstrap and is
protected by a database lease so only one replica applies it. Startup failures
from invalid documents, unresolved secrets, or failed validation stop the process.
Temporary database or provisioned blob-store readiness failures are retried while
`/readyz` remains unavailable. Startup never prunes.

The Helm chart exposes `provisioning.document` or
`provisioning.existingConfigMap`, `provisioning.environmentSecrets`, and
`provisioning.secretMounts`. Extra OCI Service ports and Ingress hosts are derived from
`endpoints` on repositories in an inline `provisioning.document`. See the chart README
for a complete values example.

## Examples

The [`examples/`](../../examples/) directory holds ready-to-apply documents for
common use cases — a container registry, hosted language registries, a Go module
proxy, an AUR mirror, and a full stack — each with a README covering the client
commands that go with it.
