# Webhooks (event notifications)

suxen delivers signed HTTP notifications when assets change. This example runs a
**dedicated support stack** — a small HMAC-verifying receiver — and a
natively-run suxen configured to deliver `asset.uploaded` and `asset.deleted`
events for the raw repository to it.

Each delivery is signed with HMAC-SHA256 over the body using the webhook's
`secret`, sent as the `X-Suxen-Signature-256` header; the receiver verifies it
with the same secret. Deliveries are asynchronous and retried, with a
dead-letter state after repeated failures.

## Server side

```sh
examples/webhooks/start.sh
```

This builds and starts [`compose.yaml`](compose.yaml) (the receiver on
`127.0.0.1:9080`), then runs the launcher with [`repo.yaml`](repo.yaml), which
declares the webhook. Because the receiver listens on the loopback address —
which suxen's outbound client denies by default as SSRF protection — `start.sh`
sets `SUXEN_OUTBOUND_ALLOWED_HOSTS=127.0.0.1`. Ctrl-C stops suxen and tears the
stack down.

The webhook `secret` is the resource's kind-specific secret field: it is
stripped from the stored spec and never returned by the API. It is inline in
`repo.yaml` here so the example applies with no external input; in production
supply it from the environment with a `secretRef`.

## Client side

In a separate client shell, export `SUXEN_URL` and `SUXEN_TOKEN` using the URL
and admin token printed by `start.sh`.

The "client" is your receiver — any HTTP endpoint that verifies the signature.
Trigger a delivery by changing an asset:

```sh
printf 'webhook event\n' > event.txt
curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file event.txt \
  "$SUXEN_URL/repository/raw/webhook/event.txt"
```

Inspect deliveries from suxen's side, and manage webhooks, with the admin client:

```sh
suxenctl webhook list
suxenctl webhook deliveries raw-events
```

This example's receiver also records what it got at `GET http://127.0.0.1:9080/events`
(and `DELETE` clears it).

## Test

[`test.bats`](test.bats) uploads a raw asset and waits for the signed
`asset.uploaded` event to reach the receiver. It needs Docker (for the receiver
and the `curl` client). Run it with `bats examples/webhooks/test.bats`.
