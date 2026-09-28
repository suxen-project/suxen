The validity rows were generated with Rust semver 1.0.28. Go tests read the
checked-in TSV and do not need Rust. From `plugins/format/cargo`, regenerate
using the pinned Cargo lockfile:

```sh
CARGO_TARGET_DIR=/tmp/suxen-cargo-oracle-target cargo run --locked --quiet --manifest-path testdata/native-validity/Cargo.toml > testdata/cargo-semver-1.0.28-validity.tsv
```

Add `--offline` when the dependency is already cached locally.
