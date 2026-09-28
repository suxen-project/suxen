# Release procedure

Releases are built only by the public GitHub workflow from a GitHub-verified, annotated
`v*` tag. The tag supplies the one version used by both binaries, every archive name,
the runtime and SDK image tags, the chart `version` and `appVersion`, and the GitHub
release.

The canonical repository is [`suxen-project/suxen`](https://github.com/suxen-project/suxen),
and its default branch is `main`. Public CI runs on pull requests, pushes to `main`, and
`v*` tags. Default-branch protection must require the workflow's `source`,
`release-contract`, `windows-filesystem`, `sdk`, `examples`, `interoperability`,
`storage-backends`, and `vulnerability-scan` checks.

For the first source publication, push the reviewed root commit to `main` and verify
that the resulting **push** run of `public-ci` completes successfully before tagging.
Check that GitHub's default branch and branch protection apply to `main`; a successful
push or a pull-request run alone does not prove the default-branch workflow ran.

Before tagging, finalize the candidate entry in `CHANGELOG.md`, match the
checked-in chart version to the intended tag, and run the complete release
matrix. Record the contract surface versions this release ships —
`make print-contract-matrix` renders them from
`internal/contract/versions.yaml` — in the release notes, so each application version
documents the SPI, HTTP API, and webhook API versions it contains. The artifact contract can be
reproduced without publishing anything:

```sh
scripts/build-release-artifacts.sh 1.0.0-rc.1 dist
scripts/check-release-artifacts.sh 1.0.0-rc.1 dist
```

The checker unpacks all six OS/architecture archives, verifies their files, embedded
version, and Go `GOOS`/`GOARCH` metadata, then executes the two binaries for the native
Linux architecture. Canonical GitHub CI installs pinned QEMU/binfmt support and sets
`SUXEN_RELEASE_EXECUTE_LINUX_TARGETS=amd64,arm64`, so both Linux archives execute there.
The pull-request image gate likewise builds and boots the runtime image and a custom
distribution produced by the SDK image for linux/amd64 and linux/arm64. The arm64 runs
are emulated; they are executable coverage, not evidence from native ARM hardware.

Create and push a signed annotated tag only after the candidate commit is approved and
its push checks on `main` have passed. The `github` remote below must point to the
canonical repository. Fetch its current `main` and tag that exact approved commit:

```sh
git fetch github main
git switch --detach github/main
git tag --sign v1.0.0-rc.1 -m "Suxen v1.0.0-rc.1"
git push github v1.0.0-rc.1
```

Release-candidate tags (for example `v1.0.0-rc.1`) are marked with GitHub's pre-release
flag and publish immutable versioned packages; they do not move the `latest` image tag.
The first `v1.0.0-rc.1` release uses the concise `CHANGELOG.md` feature summary as its
GitHub release notes. Later tags use generated notes. A stable release promotes the
already signed and attested runtime and SDK image digests to
`latest` only after the complete public acceptance stage succeeds. Promotion does not
rebuild either image, and the workflow verifies that each alias resolves to the accepted
digest. The registry cannot move aliases in two repositories atomically, so do not
announce a stable release until this final step succeeds; rerunning it safely converges
both aliases after an interrupted promotion.

The tag workflow builds Linux, macOS, and Windows archives for amd64 and arm64, checks
their SHA-256 manifest and target metadata, executes both Linux architectures, and checks
the packaged chart metadata. It publishes linux/amd64 and linux/arm64 runtime and SDK
images, plus the chart, using immutable version tags. Keyless Cosign signatures and
GitHub provenance attestations cover the checksum manifest, release files, both image
indexes, and the chart digest.

GitHub's supported package settings must mark `suxen`, `suxen-sdk`, and `suxen-chart`
public before a release can proceed. In each package, open **Package settings**, choose
**Change visibility**, and select **Public**. The workflow never attempts to mutate
visibility through the Packages REST API because GitHub does not provide a supported
visibility-update endpoint.

For the first publication, the packages do not exist until the tag job pushes them. That
initial job is expected to stop at the anonymous-access gate before creating the GitHub
release. An organization owner must then make all three packages public through their
settings pages and rerun the failed job. Later releases require the same public visibility
before tagging. The gate uses isolated empty Docker and Helm credential stores to fetch
both image manifests and the chart anonymously.

After the anonymous package gate passes, the workflow creates the GitHub release and
downloads every release asset through unauthenticated GitHub URLs. It rechecks the
checksum signature and GitHub attestations, requires both image platforms, anonymously
pulls and starts each runtime image, and anonymously builds and starts a custom
distribution from each matching SDK image. A disposable Kind cluster installs the
published chart, pulls its default version-matched image, reaches readiness, and reports
the release version. Any failure leaves the release job red.

The workflow runs the same public acceptance script that an operator can rerun before
announcing the release. It requires Cosign, Docker with amd64/arm64 binfmt support, Go,
the GitHub CLI, Helm, `jq`, `kubectl`, archive tools, and a current Kubernetes context:

```sh
scripts/verify-published-release.sh 1.0.0-rc.1 suxen-project/suxen
```

The macOS and Windows archives are cross-compiled and structurally inspected on the
Linux release runner; the Windows/amd64 source job separately executes filesystem blob
and resumable-upload round trips on a native Windows runner. The packaged Windows
archives and all macOS archives are not executed natively by this workflow. The public
acceptance stage runs against a published release:
the GitHub signing identity, anonymous package visibility, public download URLs, registry
attestations, and installed OCI chart are properties of the published objects.

After a stable release, restore an empty `Unreleased` section in `CHANGELOG.md`. Release
tags originate from and are published by the canonical GitHub repository.

The release workflow independently resolves the annotated tag and canonical `main`
through the GitHub API and refuses publication unless both point to the same commit.
