The validity rows were generated with Node semver 7.8.5. Go tests read the
checked-in TSV and do not need Node. From `plugins/format/npm`, regenerate with
an existing installation:

```sh
NODE_PATH=/path/to/node_modules node testdata/generate-native-validity.js > testdata/npm-semver-7.8.5-validity.tsv
```

If semver is not installed, first install the pinned package into a temporary
directory, then set `NODE_PATH` to that directory's `node_modules`:

```sh
oracle_dir=$(mktemp -d)
npm install --prefix "$oracle_dir" --ignore-scripts --no-audit --no-fund semver@7.8.5
NODE_PATH="$oracle_dir/node_modules" node testdata/generate-native-validity.js > testdata/npm-semver-7.8.5-validity.tsv
```
