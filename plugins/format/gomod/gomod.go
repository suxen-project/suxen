// Package gomod is the Go module proxy repository format plugin.
//
// It teaches suxen the GOPROXY protocol layout. Proxy repositories cache an
// upstream module proxy (proxy.golang.org, another suxen) and serve it to
// `go` with GOPROXY pointed at the repository: version lists, @latest, and
// info files for non-canonical version queries (branches, commit hashes)
// are revalidated on the proxy TTL; .info for canonical versions, .mod and
// .zip files are cached immutably. Requests under sumdb/ are forwarded to
// the upstream and revalidated on the TTL, so a client that verifies against
// sum.golang.org through the proxy keeps working when the upstream proxies
// the checksum database. Hosted repositories accept .info, .mod, and .zip
// uploads for canonical versions and synthesize @v/list and @latest once all
// three files for a version are present; group repositories merge version
// lists and choose @latest across members by preferring releases, then
// prereleases, then pseudo-versions (ordered by commit time), while module
// files resolve first-match.
//
// Module coordinates are projected into the reserved "go" attribute namespace
// as go.module and go.version (case-unescaped, so cleanup policies select on
// the real module path).
//
// The repository takes no formatConfig. The package registers the "go"
// format on import and only depends on the public suxen SPI.
package gomod

import (
	"fmt"
	"strings"

	"golang.org/x/mod/module"

	"github.com/suxen-project/suxen/spi/format"
)

func init() {
	format.Register(Format{})
}

// Format implements the Go module proxy repository format.
type Format struct{}

var (
	_ format.RepositoryValidator = Format{}
	_ format.UploadPolicy        = Format{}
	_ format.AttributeProjector  = Format{}
	_ format.ProxyPolicy         = Format{}
	_ format.GroupMerger         = Format{}
	_ format.HostedSynthesizer   = Format{}
	_ format.RetentionGrouping   = Format{}
	_ format.RetentionUnitPaths  = Format{}
)

// Name returns the format identifier.
func (Format) Name() string { return "go" }

// RetentionGroupKey makes versions of one module compete for keepLast.
func (Format) RetentionGroupKey(_ format.Repository, asset format.Asset) (string, bool) {
	info, ok := parsePath(asset.Path)
	if !ok || !info.kind.isModuleFile() || !validModuleVersion(info.module, info.version) {
		return "", false
	}
	return info.module, true
}

// CompanionPaths has no metadata companions: all three files are artifacts.
func (Format) CompanionPaths(_ format.Repository, _ string) []string { return nil }

// RetentionUnitPaths names the three files required for a usable version.
func (Format) RetentionUnitPaths(_ format.Repository, assetPath string) []string {
	info, ok := parsePath(assetPath)
	if !ok || !info.kind.isModuleFile() || !validModuleVersion(info.module, info.version) {
		return nil
	}
	escapedModule, err := module.EscapePath(info.module)
	if err != nil {
		return nil
	}
	escapedVersion, err := module.EscapeVersion(info.version)
	if err != nil {
		return nil
	}
	base := escapedModule + "/@v/" + escapedVersion
	return []string{base + ".info", base + ".mod", base + ".zip"}
}

// ValidateRepository rejects formatConfig: the format has no settings.
func (Format) ValidateRepository(repository format.Repository) error {
	for key := range repository.Config {
		return &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: fmt.Sprintf("go formatConfig does not support %q", key),
		}
	}
	return nil
}

// ValidateUpload accepts .info, .mod, and .zip files for canonical versions.
// Version lists and @latest are synthesized, so uploading them is refused.
func (Format) ValidateUpload(_ format.Repository, assetPath string) error {
	info, ok := parsePath(assetPath)
	if !ok {
		return &format.PolicyViolation{
			Code:    "go_invalid_path",
			Message: fmt.Sprintf("%q is not a Go module proxy path (<module>/@v/<version>.info|.mod|.zip)", assetPath),
		}
	}
	if !info.kind.isModuleFile() {
		return &format.PolicyViolation{
			Code:    "go_derived_path",
			Message: fmt.Sprintf("%q is derived from the stored module files and cannot be uploaded", assetPath),
		}
	}
	if !validModuleVersion(info.module, info.version) {
		return &format.PolicyViolation{
			Code:    "go_invalid_version",
			Message: fmt.Sprintf("%q is not a canonical semantic version", info.version),
		}
	}
	return nil
}

// ProjectAttributes publishes the module path and version.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	info, ok := parsePath(asset.Path)
	if !ok {
		return nil
	}
	attributes := map[string]any{"module": info.module}
	if info.kind.isModuleFile() {
		attributes["version"] = info.version
	}
	return attributes
}

// MutableUpstreamPath marks the paths whose upstream content moves: version
// lists, @latest, info files answering non-canonical queries (a branch name
// resolves to a new pseudo-version over time), and the checksum database.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	if isSumDBPath(assetPath) {
		return true
	}
	info, ok := parsePath(assetPath)
	if !ok {
		return false
	}
	switch info.kind {
	case kindList, kindLatest:
		return true
	case kindInfo:
		return !validModuleVersion(info.module, info.version)
	default:
		return false
	}
}

// validModuleVersion applies Go's module-path major-version rule and keeps
// the canonical +incompatible suffix used by legacy v2+ modules.
func validModuleVersion(modulePath, version string) bool {
	return module.Check(modulePath, version) == nil && module.CanonicalVersion(version) == version
}

type pathKind uint8

const (
	kindList pathKind = iota + 1
	kindLatest
	kindInfo
	kindMod
	kindZip
)

func (kind pathKind) isModuleFile() bool {
	return kind == kindInfo || kind == kindMod || kind == kindZip
}

// pathInfo is one parsed GOPROXY path. module and version are unescaped.
type pathInfo struct {
	module  string
	version string
	kind    pathKind
}

// parsePath recognizes <escaped-module>/@v/list, <escaped-module>/@latest,
// and <escaped-module>/@v/<escaped-version>.{info,mod,zip}.
func parsePath(assetPath string) (pathInfo, bool) {
	assetPath = strings.TrimPrefix(assetPath, "/")
	if escapedModule, found := strings.CutSuffix(assetPath, "/@latest"); found {
		return newPathInfo(escapedModule, "", kindLatest)
	}
	escapedModule, file, found := strings.Cut(assetPath, "/@v/")
	if !found || strings.Contains(file, "/") {
		return pathInfo{}, false
	}
	if file == "list" {
		return newPathInfo(escapedModule, "", kindList)
	}
	for extension, kind := range map[string]pathKind{".info": kindInfo, ".mod": kindMod, ".zip": kindZip} {
		if escapedVersion, found := strings.CutSuffix(file, extension); found {
			return newPathInfo(escapedModule, escapedVersion, kind)
		}
	}
	return pathInfo{}, false
}

func newPathInfo(escapedModule, escapedVersion string, kind pathKind) (pathInfo, bool) {
	modulePath, err := module.UnescapePath(escapedModule)
	if err != nil {
		return pathInfo{}, false
	}
	info := pathInfo{module: modulePath, kind: kind}
	if kind.isModuleFile() {
		if escapedVersion == "" {
			return pathInfo{}, false
		}
		version, err := module.UnescapeVersion(escapedVersion)
		if err != nil {
			return pathInfo{}, false
		}
		info.version = version
	}
	return info, true
}

func isSumDBPath(assetPath string) bool {
	assetPath = strings.TrimPrefix(assetPath, "/")
	return strings.HasPrefix(assetPath, "sumdb/")
}
