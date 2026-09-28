// Regenerate from plugins/format/cargo with:
// CARGO_TARGET_DIR=/tmp/suxen-cargo-oracle-target cargo run --offline --quiet \
//   --manifest-path testdata/native-validity/Cargo.toml \
//   > testdata/cargo-semver-1.0.28-validity.tsv
fn main() {
    let versions = [
        "0.0.0",
        "18446744073709551615.0.0",
        "0.18446744073709551615.0",
        "0.0.18446744073709551615",
        "18446744073709551616.0.0",
        "0.18446744073709551616.0",
        "0.0.18446744073709551616",
        "1.0.0-18446744073709551616",
        "1.2.3-alpha.1+build",
        "01.2.3",
    ];
    for candidate in versions {
        println!("{}\t{}", candidate, semver::Version::parse(candidate).is_ok());
    }
}
