# Classification (labelling assets)

A classification tags assets as they are stored, by matching their projected
attributes against ordered rules. The labels land under the reserved
`classification.` attribute namespace and feed other policies — cleanup criteria
and download gates can match on them. Classification never blocks anything; it
labels.

## Server side

```sh
examples/classification/start.sh
```

Builds and runs a local suxen (SQLite + in-memory blob store) and applies
[`repo.yaml`](repo.yaml), which binds a classification to the bootstrap `raw`
repository: assets under `releases/` get `classification.tier=release`, assets
under `snapshots/` get `classification.tier=snapshot`. Later rules win on the
same key, and a rule change relabels existing assets.

## Client / operator side

In a separate client shell, export `SUXEN_URL` and `SUXEN_TOKEN` using the URL
and admin token printed by `start.sh`.

Upload assets normally; the label is applied automatically:

```sh
printf 'sample build\n' > app.bin
curl -H "Authorization: Bearer $SUXEN_TOKEN" --upload-file app.bin \
  "$SUXEN_URL/repository/raw/releases/app.bin"
```

Inspect an asset's labels with the admin client (find the id with
`suxenctl repo assets raw <prefix>`):

```sh
suxenctl attribute get raw <asset-id> classification
# -> {"classification.tier":"release"}
```

## Test

[`test.bats`](test.bats) uploads under both prefixes and asserts each asset
received the right `classification.tier` label. Hermetic (suxenctl + a pinned
`curl` image); needs only Docker. Run it with `bats
examples/classification/test.bats`.
