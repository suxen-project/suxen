# Release test matrix

This matrix records what each test tier actually proves. A unit or contract test checks
an in-process invariant, an executable example boots one native single-node server, a
native-client scenario drives an external client, and a deployment scenario uses the
three-replica PostgreSQL/MinIO topology. These levels are complementary; a row does not
imply that every format, policy, storage driver, and deployment combination was tested.

The checked-in release workflow is configured to reject a release candidate when the
full interoperability or executable-example run skips a scenario.

Rows below describe checked-in test coverage and configured jobs. Record successful
job results against the exact candidate commit before making a release claim; a
configured job or cross-compilation alone is not evidence that the candidate ran on
that platform. Native Windows coverage is limited to filesystem blob, upload-session,
and generic staging ownership/reclamation tests, and no
native macOS job is configured. See the [platform scope](../reference/compatibility.md#deployment-topology).

| Capability | Source and contract evidence | Executable evidence and current boundary |
| --- | --- | --- |
| Raw hosted, proxy, group | `internal/server` and `internal/content` protocol tests, including repeated conditional headers and concurrent proxy cache result ordering | Single-node hosted example; three-replica hosted/S3 dedup, credentialed proxy, and proxy-member group scenarios |
| OCI push, pull, groups, referrers, ranges | `internal/oci`, `internal/content`, and server protocol tests, including multiple upstream authentication challenges and quoted pagination links across catalog, tags, and referrers | Single-node ORAS, Docker, and Helm round-trips; three-replica Docker/Crane/ORAS/Cosign scenarios |
| Maven hosted, proxy, group | `plugins/format/maven` contracts, including a reproducible direct-pair Maven oracle for ASCII and Unicode versions, generated latest/release metadata, same-second snapshot build selection, and ambiguous metadata paths across synthesis, policy, and retention | Single-node Maven deploy/resolve; three-replica Maven and Gradle publication, resolution, and write rejection |
| Go modules hosted, proxy, group | `plugins/format/gomod` hosted/list/latest, proxy/cache, and group merge tests | Single-node Go example downloads through the proxy and resolves a module uploaded to the hosted member through the group |
| Cargo hosted, proxy, group | `plugins/format/cargo` contracts, including native SemVer numeric bounds, generated metadata size boundaries, atomic oversized-publication rejection, expanded stored-entry reads and retries, exact indexed version downloads and current cache-generation visibility in group inventory and asset-ID downloads | Single-node `cargo publish`/fetch hosted round-trip, hermetic hosted-plus-proxy group fetch, and crates.io proxy fetch |
| npm hosted, proxy, group | `plugins/format/npm` contracts, including native SemVer numeric and length bounds, historical package names and native installs, large packuments, full metadata across mixed client requests, stable latest tags for equal-precedence versions, and current cache-generation visibility in group inventory and asset-ID downloads | Single-node `npm publish`/pack hosted round-trip, hermetic hosted-plus-proxy group pack, and npmjs.org proxy fetch |
| PyPI hosted, proxy, group | `plugins/format/pypi` contracts, including bounded URL expansion and rendering, large root indexes, group quarantine and index/download consistency, required JSON schema fields, fileless releases, project status, provenance links, advertised signature companions, hash-preserving HTML/JSON conversion, metadata flags, repeated Accept fields and preferences, and HTML base URL downloads | Single-node Twine/pip hosted round-trip, hermetic hosted-plus-proxy group download, and pypi.org proxy fetch |
| Git snapshot proxy | `plugins/format/git` snapshot contracts | Single-node shallow clones for GitHub and AUR sources; no Git hosting or push support |
| Authentication and RBAC | `internal/identity` and server route tests, including local account identity binding across account deletion/recreation and concurrent authorization | Single-node scoped-token example; three-replica private default, scope, revocation, and `whoami` scenarios |
| OIDC | Mock-provider server contracts cover password grant and authorization code with PKCE, callback, cookie session, and logout | Chromium completes an authorization-code login with PKCE against a local mock provider, checks the cookie session, and logs out. The Dex example exercises password grant and role mapping; browser SSO against Dex or a production IdP is not covered |
| Download gates and trust policies | provenance, content, and server tests | Single-node Raw gate/trust examples and three-replica OCI/Cosign scenarios; supported policy combinations remain independently scoped |
| Webhooks and scanner quarantine | store, content, and server transaction tests | Single-node scanner workflow; three-replica HMAC verification, retry, and dead-letter scenarios |
| Filesystem, S3, GCS blob stores | blob conformance and driver integration tests; Windows/amd64 and Windows/arm64 filesystem tests compile | Single-node filesystem/named-store and MinIO examples; GitHub CI executes filesystem blob, resumable-upload, and generic staging ownership/reclamation tests on native Windows/amd64; CI uses MinIO for S3 and an emulator for GCS, not production cloud IAM |
| Schema migration | immutable migration checksums, SQLite/PostgreSQL final-shape tests, preservation of existing cache rows through migration 0011 to 0012, timestamp normalization through migration 0013 with lease and conditional-update checks, and migration 0014 preserving account credentials and roles while assigning stable identities | Three fresh processes race the initial migration on an empty PostgreSQL database; no online version-to-version upgrade is exercised |
| Garbage collection and blob-store drain | deterministic store/server lease, write-redirection, migration, and GC tests | Single-node named-store example drains one store and runs GC; no multi-replica drain rehearsal is present |
| Provisioning | strict document/engine tests, including exact numeric predicates, complete replacement of supplied fields, OIDC group-role revocation, and rejection of values that cannot be encoded as JSON | Single-node dry-run, apply, idempotence, prune, ownership, secret references, exact numeric partial updates, and invalid-document rejection |
| Backup and restore | A server-level SQLite/filesystem test stops the source, copies the paired data directory, and boots the restored server | The storage-backend job clones a stopped PostgreSQL recovery point and copies committed blobs to an isolated MinIO prefix; both scenarios verify probes, Raw/OCI bytes, scoped token and control-plane state, and rollback exclusion of a later write |
| Administration UI | JavaScript DOM/helper tests, OpenAPI shape checks, and server security contracts | Chromium exercises token sign-in, repository create/edit/delete, exact numeric policy edits in regular and advanced forms, logout, OIDC callback/session/logout against a mock provider, same-origin cookie mutation, cross-origin CSRF rejection, anonymous rendering, artifact-origin isolation, and native cancel |
| External plugins and SDK image | SPI registry/conformance suites plus a separate module that implements blob, format, and control-plane API consumers | The SDK compiles and boots the custom distribution, including `suxencmd.Main` and the default plugin import surface |
| Release archives and images | Artifact scripts verify names, checksums, archive members, embedded versions, and every target's Go OS/architecture metadata | Linux/amd64 and emulated arm64 binaries execute, and the pull-request gate boots runtime and SDK-derived images on both; native Windows/amd64 CI executes the filesystem storage path but not the release archive; the tag acceptance gate verifies public signatures, attestations, anonymous downloads, and a chart install. macOS and Windows archives are not executed natively |

The workflow also runs source/race/build-tag checks, the separate plugin module, pinned
dependency scanning, and runtime plus SDK image scans. This describes configured gates;
public tag publication and download verification remain separate release evidence.

Dependency scans report called-symbol and imported-package findings separately
from module-only advisories. Review that reachability distinction when interpreting
`govulncheck` output. Image scans cover both the runtime and SDK images and reject
high or critical findings. Scan reports are release-specific evidence; the configured
scanner versions and commands are recorded in the CI workflow.
