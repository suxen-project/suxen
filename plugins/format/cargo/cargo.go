// Package cargo is the Cargo (crates.io-style) sparse registry format plugin.
//
// A cargo proxy repository caches an upstream sparse registry index such as
// https://index.crates.io and the crate files it points at. Cargo reaches it
// as `sparse+<repository URL>/`: the registry config.json and the per-crate
// index files are revalidated on the proxy TTL; crate files are cached
// immutably at dl/<crate>/<version>/download. The upstream config.json is
// stored verbatim and rewritten on the way out so its `dl` template points at
// this repository under the hostname the client used; the crate download then
// resolves through that cached upstream template (crates.io serves crates from
// static.crates.io, a different host than the index), so the whole flow stays
// inside suxen. The `api` entry is dropped because publish, yank, and search
// are not proxied.
//
// Group repositories merge index files across members (one entry per version,
// first member wins) and serve a config.json pointing at the group.
//
// Hosted repositories accept `cargo publish` over the WireProtocol hook: the
// publish PUT (api/v1/crates/new), the synthesized config.json, and the
// synthesized sparse-index files are claimed for hosted repositories only,
// while crate downloads ride the generic asset pipeline. A published crate is
// stored at dl/<name>/<version>/download with a per-version index entry; the
// index files and config are synthesized per request under the hostname the
// client used, the checksum taken from the stored crate. Cargo's bare
// Authorization token authenticates because identity accepts a scheme-less
// token as an API token.
//
// Crate coordinates are projected into the reserved "cargo" attribute
// namespace as cargo.name and cargo.version. The repository takes no
// formatConfig. The package registers the "cargo" format on import and only
// depends on the public suxen SPI.
package cargo

import (
	"fmt"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

func init() {
	format.Register(Format{})
}

// Format implements the Cargo sparse registry format.
type Format struct{}

var (
	_ format.RepositoryValidator   = Format{}
	_ format.UploadPolicy          = Format{}
	_ format.AttributeProjector    = Format{}
	_ format.ProxyPolicy           = Format{}
	_ format.IndexRewriter         = Format{}
	_ format.ProxyRequestResolver  = Format{}
	_ format.GroupMerger           = Format{}
	_ format.GroupArtifactSelector = Format{}
	_ format.WireProtocol          = Format{}
)

// Name returns the format identifier.
func (Format) Name() string { return "cargo" }

// ValidateUpload rejects generic PUTs: the native publication handler owns
// package payloads and metadata and enforces their publication invariants.
// Native publication uses WireTools directly and does not call this policy.
func (Format) ValidateUpload(_ format.Repository, _ string) error {
	return &format.PolicyViolation{
		Code:    "cargo_native_publish_required",
		Message: "hosted cargo repositories require cargo publish; direct asset uploads are not supported",
	}
}

// ValidateRepository accepts hosted, proxy, and group repositories without
// formatConfig.
func (Format) ValidateRepository(repository format.Repository) error {
	for key := range repository.Config {
		return &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: fmt.Sprintf("cargo formatConfig does not support %q", key),
		}
	}
	return nil
}

// ProjectAttributes publishes the crate name and, for crate files, the
// version.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	info, ok := parsePath(asset.Path)
	if !ok || info.kind == kindConfig {
		return nil
	}
	attributes := map[string]any{"name": info.name}
	if info.kind == kindCrate {
		attributes["version"] = info.version
	}
	return attributes
}

// MutableUpstreamPath marks the registry config and the index files, which
// change as crates are published; crate files are immutable.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	return ok && (info.kind == kindConfig || info.kind == kindIndex)
}

// RetentionGroupKey groups every version of one crate under its download
// prefix so keepLast retains the newest releases per crate, not per version.
func (Format) RetentionGroupKey(_ format.Repository, asset format.Asset) (string, bool) {
	path := asset.Path
	if base, suffix, found := strings.Cut(path, proxyIdentityMarker); found && isCargoProxyIdentity(path, base+proxyIdentityMarker) && suffix != "" {
		path = base
	}
	info, ok := parsePath(path)
	if !ok || info.kind != kindCrate {
		return "", false
	}
	return downloadPrefix + strings.ToLower(info.name), true
}

// CompanionPaths ties a crate file to the per-version sparse index entry
// published with it, so deleting the crate removes its index line too.
func (Format) CompanionPaths(_ format.Repository, assetPath string) []string {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindCrate {
		return nil
	}
	paths := []string{metadataPath(info.name, info.version), claimPath(info.name, info.version)}
	if canonical := versionIdentity(info.version); canonical != info.version {
		// Older versions also claimed this canonical index metadata path.
		paths = append(paths, metadataPath(info.name, canonical))
	}
	return paths
}

type pathKind uint8

const (
	kindConfig pathKind = iota + 1
	kindIndex
	kindCrate
)

// pathInfo is one parsed registry path.
type pathInfo struct {
	kind    pathKind
	name    string
	version string
}

const (
	configPath     = "config.json"
	downloadPrefix = "dl/"
	downloadSuffix = "/download"
)

// parsePath recognizes config.json, the sparse index layout (1/<n>, 2/<n>,
// 3/<c>/<n>, <ab>/<cd>/<n>), and dl/<crate>/<version>/download.
func parsePath(assetPath string) (pathInfo, bool) {
	assetPath = strings.TrimPrefix(assetPath, "/")
	if assetPath == configPath {
		return pathInfo{kind: kindConfig}, true
	}
	if rest, found := strings.CutPrefix(assetPath, downloadPrefix); found {
		rest, found = strings.CutSuffix(rest, downloadSuffix)
		name, version, split := strings.Cut(rest, "/")
		if !found || !split || !validCrateName(name) || version == "" || strings.Contains(version, "/") {
			return pathInfo{}, false
		}
		return pathInfo{kind: kindCrate, name: name, version: version}, true
	}
	segments := strings.Split(assetPath, "/")
	name := segments[len(segments)-1]
	if !validCrateName(name) || indexPath(name) != strings.ToLower(assetPath) {
		return pathInfo{}, false
	}
	return pathInfo{kind: kindIndex, name: name}, true
}

// indexPath is the lowercase sparse index path of a crate name.
func indexPath(name string) string {
	lower := strings.ToLower(name)
	switch len(lower) {
	case 1:
		return "1/" + lower
	case 2:
		return "2/" + lower
	case 3:
		return "3/" + lower[:1] + "/" + lower
	default:
		return lower[:2] + "/" + lower[2:4] + "/" + lower
	}
}

func validCrateName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-' || character == '_':
		default:
			return false
		}
	}
	return true
}
