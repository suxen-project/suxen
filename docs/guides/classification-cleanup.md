# Classification and cleanup

Classification labels artifacts as they are written; cleanup policies act on those labels,
access recency, and attributes to reclaim space. Both use the same typed predicate model
described in the [API guide](../reference/api.md).

## Classification rules

Classification rules run in order against each asset's projected attributes. Each rule has
`when` (a list of `{path, op, value}` predicates that must all match — an empty list always
matches) and assigns the namespaced label `classification.<key> = <value>`. Every matching
rule contributes its label, a later rule overwrites an earlier one on the same key, and an
asset matched by no rule receives no `classification.*` labels. Updating the rules
reclassifies existing assets transactionally, and later writes are classified as part of
their metadata commit. Labels are exposed as reserved, read-only `classification.<key>`
attributes. Asset responses also expose virtual `sys.*` fields and Raw or OCI coordinates
derived from authoritative metadata; these fields are not duplicated in stored attribute
JSON. Rule evaluation uses complete persisted asset metadata, including the public path
of cached packages and access and validation timestamps. Previous classification labels
are excluded from the rule input, so reapplying rules does not feed their old output back
into the calculation. Rules do not chain through labels assigned by earlier rules.
Relabeling preserves scanner and verification annotations written concurrently.
For example, save this as `classification.json`:

```json
{
  "rules": [
    {"when": [{"path": "sys.path", "op": "matches", "value": "-(alpha|beta|rc[0-9]*)$"}], "key": "stage", "value": "prerelease"},
    {"when": [{"path": "sys.path", "op": "matches", "value": "^v?[0-9]+\\.[0-9]+\\.[0-9]+$"}], "key": "stage", "value": "release"},
    {"when": [{"path": "sys.size", "op": ">", "value": 1073741824}], "key": "weight", "value": "large"}
  ]
}
```

### Instance-wide default

A single instance-wide default set of rules can back every repository, so a common labelling
scheme is declared once:

```sh
./bin/suxenctl classification defaults set classification.json
```

A repository inherits the default unless it opts out. When it inherits, the default's rules
are evaluated **before** its own, so a repository rule wins on a shared key. The
`inheritGlobal` flag on a repository's classification controls this (`true` by default);
setting it to `false` uses the repository's own rules alone. Because labels are materialized
at write time, changing or deleting the default retroactively reclassifies every inheriting
repository's assets. Clear it with `classification defaults delete`; with no default
configured, each repository behaves exactly as its own rules specify.

## Cleanup policies

Apply the rules and create a cleanup policy from a JSON document:

```json
{
  "name": "discard-prereleases",
  "repositories": ["raw"],
  "criteria": [
    {"path": "classification.stage", "op": "=", "value": "prerelease"},
    {"path": "sys.lastAccessed", "op": "before", "value": "30d"},
    {"path": "sys.updatedAt", "op": "before", "value": "60d"}
  ],
  "keepLast": 5,
  "action": "delete",
  "enabled": false
}
```

```sh
./bin/suxenctl classification set raw classification.json
./bin/suxenctl cleanup-policy create cleanup-policy.json
./bin/suxenctl cleanup raw discard-prereleases
./bin/suxenctl cleanup --apply raw discard-prereleases
./bin/suxenctl task list
./bin/suxenctl task leader
```

A policy contains `name`, `repositories`, `criteria`, `keepLast`, `order`, `action`,
and `enabled`.
Criteria are ordered `{path, op, value}` predicates over the projected attribute view;
every predicate must match. Operators are `=`, `!=`, `<`, `<=`, `>`, `>=`, `before`,
`after`, `matches`, `contains`, `in`, `not-in`, `exists`, and `absent`. Temporal values
accept RFC3339 timestamps or relative ages using Go syntax plus `d`, `w`, `mo`, and `y`;
months and years mean fixed 30-day and 365-day periods. Reserved paths include
`sys.blobStore`, allowing aggressive cleanup of constrained stores. A policy with no
criteria is rejected, and manual cleanup is a dry-run unless `--apply` is supplied.
`sys.lastAccessed` records the latest successful download or upload; delayed
download bookkeeping cannot move it backward when requests finish out of order.

Predicate paths use dotted traversal through the same merged attributes returned by the
asset API. Missing paths match only `absent` and `not-in`; comparisons with the wrong
value type never match. Equality compares JSON objects and arrays recursively: object
key order does not matter, array order does, and JSON numbers compare by exact
decimal value: `1`, `1.0`, and `1e0` are equal, including when nested inside
objects or arrays. The same rule applies to large exponents and to `!=`, `in`,
`not-in`, `contains`, classification rules, download gates, and exact attribute search.
`keepLast` is evaluated after predicate selection and retains the
newest matching assets in each component. For Go module version files,
`keepLast` counts complete `.info`/`.mod`/`.zip` versions per module. All three
files must match the predicates before cleanup can delete that version;
incomplete versions are left for explicit deletion. Other Go proxy paths such
as `@v/list` and `@latest` continue to follow ordinary asset retention.
For Maven, `keepLast` counts version directories per group and artifact ID;
the JAR, POM, classifiers, checksums, and signatures in one directory are
retained or deleted together. Every stored file in that directory must match
the policy, including SNAPSHOT version-level `maven-metadata.xml` if present; a partial
match leaves the entire version alone. Timestamped SNAPSHOT builds in one
`-SNAPSHOT` directory count as one version. A directory needs an artifact file
(not only metadata, checksums, or signatures) to form a version unit. Standalone
metadata uses ordinary retention within its own parent directory, including
artifact-level indexes for artifact IDs ending in `-SNAPSHOT`.

`order` decides which entries `keepLast` retains in each component. `updatedAt`, the
default, keeps the most recently updated entries. `version` keeps the highest Raw
component versions or OCI tags. Two versions that both parse as semantic versions,
with or without a leading `v`, compare by semver precedence; otherwise they compare in
natural order, so `build-10` follows `build-9`. Equal versions fall back to update
time. With `version`, an entry without a version ranks below every versioned entry;
formats without versions effectively keep `updatedAt` ordering.

### Raw component retention

A Raw repository with [component patterns](repositories.md#raw-components) groups
matched assets by `raw.component`. A version directory is one unit: every file in it
must match the policy, `keepLast` counts directories, and cleanup deletes the whole
directory in one transaction. A file added or changed after selection keeps the entire
directory. For example, with the `models` pattern and `.glb` anchor from the
repositories guide, and these assets:

```text
models/blocksets/core/0.9.0/core.glb
models/blocksets/core/0.9.0/SHA256SUMS
models/blocksets/core/0.10.0/core.glb
models/blocksets/core/0.10.0/SHA256SUMS
models/blocksets/core/0.11.0/SHA256SUMS
```

a classification rule labels each payload and its checksums alike:

```json
{"rules": [{"when": [{"path": "raw.component", "op": "matches", "value": "^models/"}], "key": "kind", "value": "model"}]}
```

and this policy keeps only the highest complete model version:

```json
{
  "name": "keep-latest-model",
  "repositories": ["models"],
  "criteria": [{"path": "classification.kind", "op": "=", "value": "model"}],
  "keepLast": 1,
  "order": "version",
  "action": "delete",
  "enabled": true
}
```

Cleanup deletes both files of `0.9.0` and keeps `0.10.0`, even if `0.9.0` was uploaded
later. `0.11.0` contains no anchor file, so it is not a version and does not take the
retained slot; its checksum file follows ordinary retention within the component. If the
criteria matched only `core.glb`, no directory would be complete and nothing would be
deleted.

## Scheduling and garbage collection

Enabled policies run every `SUXEN_CLEANUP_INTERVAL`; blob collection runs independently
every `SUXEN_GC_INTERVAL`. Each singleton scheduler has its own renewable database lease,
so disabling cleanup never disables physical blob reclamation. Every manual or scheduled
run is recorded in `/api/v1/tasks`. OCI manifests persist their config, layer,
child-manifest, and subject dependencies, so deleting the last eligible tag can safely
release the canonical manifest and eventually its unshared blobs. Blob deletion still
observes the GC grace period to protect concurrent uploads. On-demand garbage collection
and grace handling are covered in the [operations guide](../operations/operations.md).
