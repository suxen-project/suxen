# Webhook-driven scanning (quarantine → scan → release)

The end-to-end "upload, quarantine until an external scanner clears it" workflow:
a download gate withholds an asset until it carries a `scan.status=passed`
attribute, a webhook notifies an external scanner when an asset appears, and the
scanner stamps that attribute back through the suxen API. It works the same for a
**hosted** upload and a **proxy** pull (the first fetch is what the scanner sees).

## How a webhook creates attributes

suxen webhook delivery is **asynchronous and one-way**: the event is queued and
delivered at-least-once with retries and a dead-letter queue, and suxen reads only
the receiver's HTTP status (2xx = delivered, non-2xx = retry). **suxen does not
read attributes out of the webhook response.** So a receiver that wants to
annotate an asset does it the other way around: it calls the suxen attribute API.

```
PUT /api/v1/repositories/{repo}/assets/{id}/attributes/{namespace}
If-Match: sha256:<asset-digest-from-event>
{ "status": "passed" }
```

Two design points make this safe:

- **Dedicated privilege.** Writing an attribute needs `repository:<repo>:annotate`
  — *not* `write`. The scanner's service-account token is scoped to `annotate`
  only, so it can stamp attributes but cannot push or delete artifacts. The test
  asserts this (annotate 200, push 403).
- **Dedicated namespace.** Attributes are namespaced; the scanner writes its own
  (`scan` here). System namespaces (`provenance`, `sys`, …) are reserved and
  rejected for external writes, so a scanner cannot forge provenance verdicts.
- **Generation precondition.** The scanner copies `asset.digest` from the signed
  event into `If-Match`. If that path has since been replaced, Suxen rejects the
  delayed result instead of applying it to different bytes.

The scanner in this example is a small service ([the e2e support
`webhook-enricher`](../../test/e2e/support/main.go)): it verifies the HMAC, reads
`repository`, `asset.id`, and `asset.digest` from the event, and PUTs the
attribute back with the digest precondition.

> **A tighter integration is planned but not yet built:** an in-process event
> handler SPI (`spi/events`) whose `Toolbox.SetAttributes(namespace, values)`
> lets a scanner annotate directly — same durable queue as webhooks, no callback
> token or HTTP round-trip. Until it lands, the service-account callback shown
> here is the supported path.

## A note on client retries

The gate returns a hard **403** while an asset is withheld — there is no
"try again later" retryable status today, so `curl`/`docker`/`git` will not retry
transparently. A client (or CI step) that pulls a freshly uploaded/fetched
artifact must **retry on 403** until the scan completes (typically a second or
two). This example's test polls the download until it turns 200.

## Server side

```sh
examples/webhook-scanner/start.sh
```

Brings up the scanner and a tiny upstream ([`compose.yaml`](compose.yaml)), runs
a local suxen, applies [`repo.yaml`](repo.yaml) (repos `uploads` hosted + `mirror`
proxy, a `scan.status=passed` gate on each, the `scanner` role, and the webhook),
then mints an `annotate`-scoped token and hands it to the scanner. Because the
scanner and upstream live on loopback, `start.sh` sets
`SUXEN_OUTBOUND_ALLOWED_HOSTS=127.0.0.1`.

## Client side

```sh
# Hosted: push an artifact; it is withheld until the scan lands, then served.
curl -H "Authorization: Bearer $SUXEN_TOKEN" -X PUT --upload-file app.bin \
  "$SUXEN_URL/repository/uploads/app.bin"
curl -o app.bin --retry 10 --retry-all-errors \
  "$SUXEN_URL/repository/uploads/app.bin"       # 403 until scanned, then 200

# Proxy: the first pull fetches from upstream, then the same quarantine applies.
curl --retry 10 --retry-all-errors "$SUXEN_URL/repository/mirror/artifact.txt"
```

## Test

[`test.bats`](test.bats) asserts: a hosted upload is withheld (403) then released
(200) once the scanner stamps `scan` (attribute present); a proxy first-pull is
fetched, withheld, then released and serves the upstream bytes; and the scanner's
`annotate`-scoped token can set attributes (200) but cannot push (403). Hermetic
(the upstream is a mounted fixture); needs Docker. Run it with
`bats examples/webhook-scanner/test.bats`.
