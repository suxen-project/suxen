// Package npm is the npm registry format plugin.
//
// An npm proxy repository caches an upstream registry such as
// https://registry.npmjs.org and is used with `npm config set registry
// <repository URL>/`. Packuments (the per-package metadata documents) are
// revalidated on the proxy TTL and rewritten on the way out so every
// `dist.tarball` points back at this repository under the hostname the client
// used. The proxy requests and caches full upstream packuments for all client
// Accept headers; tarballs are cached immutably at
// <name>/-/<name>-<version>.tgz. When
// a tarball is requested the upstream URL is taken from the cached packument,
// so registries that serve tarballs from another host than the metadata keep
// working. After a packument is deleted, a cached tarball remains readable
// when it has one unambiguous source identity; otherwise the tarball is fetched
// from the upstream at the same path.
//
// Group repositories merge packuments across members: versions are unioned
// (first member wins per version), `latest` is the highest release across
// members, other dist-tags and metadata come from the first member that has
// them.
//
// Hosted repositories accept `npm publish` over the WireProtocol hook: the
// publish PUT and the packument GET are claimed for hosted repositories only,
// while tarball downloads ride the generic asset pipeline. A published version
// is stored as its tarball at <name>/-/<file>.tgz plus a small per-version
// metadata asset at <name>/-/metadata/<version>.json; the packument is
// synthesized per request from those, with every dist.tarball absolute under
// the hostname the client used and dist.integrity/shasum passed through
// untouched. `dist-tags.latest` is the highest published release; custom
// publish tags are not preserved.
//
// Package coordinates are projected into the reserved "npm" attribute
// namespace as npm.name and npm.version. The repository takes no
// formatConfig. The package registers the "npm" format on import and only
// depends on the public suxen SPI.
package npm

import (
	"fmt"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

func init() {
	format.Register(Format{})
}

// Format implements the npm registry format.
type Format struct{}

var (
	_ format.RepositoryValidator  = Format{}
	_ format.UploadPolicy         = Format{}
	_ format.AttributeProjector   = Format{}
	_ format.ProxyPolicy          = Format{}
	_ format.IndexRewriter        = Format{}
	_ format.ProxyRequestResolver = Format{}
	_ format.GroupMerger          = Format{}
	_ format.WireProtocol         = Format{}
)

// Name returns the format identifier.
func (Format) Name() string { return "npm" }

// ValidateUpload rejects generic PUTs: the native publication handler owns
// package payloads and metadata and enforces their publication invariants.
// Native publication uses WireTools directly and does not call this policy.
func (Format) ValidateUpload(_ format.Repository, _ string) error {
	return &format.PolicyViolation{
		Code:    "npm_native_publish_required",
		Message: "hosted npm repositories require npm publish; direct asset uploads are not supported",
	}
}

// ValidateRepository accepts hosted, proxy, and group repositories without
// formatConfig.
func (Format) ValidateRepository(repository format.Repository) error {
	for key := range repository.Config {
		return &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: fmt.Sprintf("npm formatConfig does not support %q", key),
		}
	}
	return nil
}

// ProjectAttributes publishes the package name and, for tarballs, the
// version.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	info, ok := parsePath(asset.Path)
	if !ok {
		return nil
	}
	attributes := map[string]any{"name": info.name}
	if info.kind == kindTarball {
		attributes["version"] = info.version
	}
	return attributes
}

// MutableUpstreamPath marks packuments as revalidated on the proxy TTL;
// tarballs are immutable.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	return ok && info.kind == kindPackument
}

// RetentionGroupKey groups every version of one package under its reserved "-"
// folder so keepLast retains the newest releases per package, scope included,
// not per version.
func (Format) RetentionGroupKey(_ format.Repository, asset format.Asset) (string, bool) {
	info, ok := parsePath(asset.Path)
	if !ok || info.kind != kindTarball {
		return "", false
	}
	return info.name + "/-", true
}

// CompanionPaths ties a tarball to the per-version metadata document published
// with it, so deleting the tarball removes that document too.
func (Format) CompanionPaths(_ format.Repository, assetPath string) []string {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindTarball {
		return nil
	}
	return []string{metadataPath(info.name, info.version)}
}

type pathKind uint8

const (
	kindPackument pathKind = iota + 1
	kindTarball
)

// pathInfo is one parsed registry path. name includes the scope when present.
type pathInfo struct {
	kind    pathKind
	name    string
	version string
	file    string
}

// parsePath recognizes <name> and @scope/<name> packuments and
// <name>/-/<basename>-<version>.tgz tarballs.
func parsePath(assetPath string) (pathInfo, bool) {
	segments := strings.Split(strings.Trim(assetPath, "/"), "/")
	name, rest := "", segments
	switch {
	case len(segments) >= 2 && strings.HasPrefix(segments[0], "@"):
		name, rest = segments[0]+"/"+segments[1], segments[2:]
	case len(segments) >= 1 && !strings.HasPrefix(segments[0], "@"):
		name, rest = segments[0], segments[1:]
	default:
		return pathInfo{}, false
	}
	if !validReadablePackageName(name) {
		return pathInfo{}, false
	}
	if len(rest) == 0 {
		return pathInfo{kind: kindPackument, name: name}, true
	}
	if len(rest) != 2 || rest[0] != "-" {
		return pathInfo{}, false
	}
	basename := name[strings.LastIndex(name, "/")+1:]
	version, found := strings.CutSuffix(strings.TrimPrefix(rest[1], basename+"-"), ".tgz")
	if !found || version == "" || !strings.HasPrefix(rest[1], basename+"-") {
		return pathInfo{}, false
	}
	return pathInfo{kind: kindTarball, name: name, version: version, file: rest[1]}, true
}

// Historical registry packages may have uppercase letters, exceed today's
// length limit, or use punctuation that is safe in a URL path. Reads must
// recognize those names so packuments and tarballs keep their npm behavior.
func validReadablePackageName(name string) bool {
	scope, base, scoped := strings.Cut(name, "/")
	if !scoped {
		base = scope
		scope = ""
	} else if !strings.HasPrefix(scope, "@") || len(scope) < 2 || strings.Contains(base, "/") {
		return false
	}
	for _, part := range []string{strings.TrimPrefix(scope, "@"), base} {
		if part == "" && scoped {
			return false
		}
		if part == "." || part == ".." || part == "-" {
			return false
		}
		for _, character := range part {
			switch {
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character >= '0' && character <= '9':
			case strings.ContainsRune("-_.!~*'()", character):
			default:
				return false
			}
		}
	}
	return base != ""
}

// Hosted publication keeps the modern naming policy. Legacy package names
// remain readable through proxy and group repositories but cannot be newly
// published into a hosted repository.
func validPublishPackageName(name string) bool {
	if !validReadablePackageName(name) || len(name) > 214 {
		return false
	}
	if strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "favicon.ico") {
		return false
	}
	_, base, scoped := strings.Cut(name, "/")
	if !scoped {
		base = name
		if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || strings.HasPrefix(base, "-") {
			return false
		}
	} else if strings.HasPrefix(base, ".") {
		return false
	}
	if strings.ToLower(name) != name {
		return false
	}
	for _, character := range name {
		if strings.ContainsRune("!~*'()", character) {
			return false
		}
	}
	return true
}

// tarballPath is the repository path a rewritten dist.tarball points at.
func tarballPath(name, file string) string {
	return name + "/-/" + file
}
