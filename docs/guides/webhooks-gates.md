# Webhooks and quarantine gates

Webhooks notify external services of repository events; download gates withhold assets
until an external service writes an attribute that clears them. Together they implement an
upload → scan → release workflow with no in-process plugin code.

## Webhook subscriptions

Webhook subscriptions send repository events to external scanners, notifiers, and
automation services. Save a subscription like this as `scanner-webhook.json`:

```json
{
  "name": "scanner",
  "url": "https://scanner.example/hooks/suxen",
  "secret": "replace-with-a-long-random-secret",
  "events": ["asset.uploaded", "asset.deleted"],
  "repositories": ["raw"],
  "enabled": true
}
```

```sh
./bin/suxenctl webhook create scanner-webhook.json
./bin/suxenctl webhook apply scanner scanner-webhook.json
./bin/suxenctl webhook list
./bin/suxenctl webhook deliveries WEBHOOK_NAME
```

Supported events are `asset.uploaded`, `asset.deleted`, `asset.downloaded`,
`component.created`, and `cleanup.completed`. An empty `repositories` list subscribes to
every repository. Secrets are never returned by the API; omitting `secret` from an update
preserves the stored value. `webhook apply NAME JSON_FILE` is an idempotent provisioning
operation: the command-line name is authoritative, and repeated calls retain the webhook's
creation time.

## Verifying delivery signatures

Each request body is a JSON event with a unique ID and occurrence time. The
`X-Suxen-Event` and `X-Suxen-Delivery` headers identify it. Verify
`X-Suxen-Signature-256` by computing HMAC-SHA256 over the exact request body with the
subscription secret; the header value is `sha256=<lowercase hex digest>`. `X-Suxen-Webhook-Version`
carries the SemVer of the webhook delivery contract, so a receiver can branch on payload changes.

For upload and component-created events, matching delivery rows and the asset publication
commit in the same metadata transaction. The at-least-once guarantee therefore starts when
the upload is acknowledged; a process crash cannot commit the asset without its scanner
delivery. Deliveries are leased by any available server replica.
Non-2xx responses and network failures retry with exponential backoff. After eight
attempts a delivery enters the `dead` state and remains visible in delivery history. The
Prometheus metrics include current queue and dead-letter counts.

## Download gates

A download gate supports an upload → scan → release workflow. Save this as
`scan-gate.json`:

```json
{
  "criteria": [
    {"path": "scan.status", "op": "=", "value": "passed"}
  ],
  "enabled": true
}
```

```sh
./bin/suxenctl download-gate set raw scan-gate.json
```

New assets remain downloadable only after a caller with the repository `annotate`
privilege writes the matching namespaced attribute:

```sh
curl -X PUT \
  -H "Authorization: Bearer $SUXEN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'If-Match: "sha256:ASSET_DIGEST"' \
  --data '{"status":"passed"}' \
  "$SUXEN_URL/api/v1/repositories/raw/assets/ASSET_ID/attributes/scan"
```

Gate criteria use the same predicate model as cleanup policies (see
[classification and cleanup](classification-cleanup.md)). Gates apply consistently to Raw
files, OCI manifests, and OCI blobs. A proxy gate applies to its cached assets; for a
group, configure the gate on each member that must be quarantined.

The OCI signing bootstrap exception lets authenticated publishers fetch manifests
by digest before provenance verification succeeds. It still enforces download-gate
criteria, including inherited criteria, so pending scans continue to quarantine
those manifests.

Gate updates require a non-null `criteria` array. Use `[]` to clear the criteria;
omitting the field or sending `null` returns `400` and preserves the existing gate.

### Instance-wide default

A single instance-wide default gate can back every repository, so a scan requirement is
declared once rather than per repository:

```sh
./bin/suxenctl download-gate defaults set scan-gate.json
```

A repository inherits the default unless it opts out. When it inherits, the default's
predicates are **required in addition to** its own (logical AND); a repository with no gate
of its own is gated by the default alone. The `inheritGlobal` flag on a repository gate
controls this:

- `inheritGlobal: true` (the default) — the instance default's predicates AND the
  repository's must all match.
- `inheritGlobal: false` — only the repository's own predicates apply; an empty `criteria`
  list then gates nothing, which is how a repository fully opts out of the default.

`enabled` follows the repository's gate when it has one, otherwise the default's. Clear the
default with `download-gate defaults delete`; with no default configured, each repository
behaves exactly as its own gate specifies.
