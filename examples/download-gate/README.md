# Download gate (quarantine until scanned)

A download gate withholds an asset until it satisfies ALL of the gate's
criteria. A gated download returns **403** (`download_gated`) until the
condition is met — a quarantine-until-scanned workflow. The condition is a
free-form asset attribute an operator or an external scanner sets.

## Server side

```sh
examples/download-gate/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml), which binds a gate to the `raw` repository requiring
`scan.status = passed` before an asset is served.

## Client / operator side

In a separate client shell, export `SUXEN_URL` and `SUXEN_TOKEN` using the URL
and admin token printed by `start.sh`.

An uploaded asset is initially withheld:

```sh
printf 'pending scan\n' > app.bin
curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file app.bin \
  "$SUXEN_URL/repository/raw/gated/app.bin"
curl -o /dev/null -w '%{http_code}\n' "$SUXEN_URL/repository/raw/gated/app.bin"
# -> 403
```

Release it by recording a passing scan (find the id and digest with
`suxenctl repo assets raw <prefix>`). The digest precondition prevents a delayed
scanner result from being applied after the path has been replaced:

```sh
echo '{"status":"passed"}' > scan.json
suxenctl attribute set --if-match <digest> raw <asset-id> scan scan.json
curl -o /dev/null -w '%{http_code}\n' "$SUXEN_URL/repository/raw/gated/app.bin"
# -> 200
```

Clearing the attribute
(`suxenctl attribute delete --if-match <digest> raw <asset-id> scan`)
re-quarantines it. A `downloadGate` named `default` applies a gate instance-wide;
a per-repository gate with `inheritGlobal: true` AND-extends the default's
criteria.

## Test

[`test.bats`](test.bats) uploads an asset, asserts `403`, sets `scan.status=passed`
and asserts `200`, then clears it and asserts `403` again. Hermetic (suxenctl +
a pinned `curl` image); needs only Docker. Run it with `bats
examples/download-gate/test.bats`.
