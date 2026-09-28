// Package pypi is the Python package index (PEP 503 / PEP 691) format plugin.
//
// A pypi proxy repository caches an upstream simple index such as
// https://pypi.org and the distribution files it links, even though PyPI
// serves those from files.pythonhosted.org. pip is pointed at
// `<repository URL>/simple/`. Project pages (`simple/<project>/`, HTML or
// JSON) and the root index are revalidated on the proxy TTL and rewritten on
// the way out so every file link points back at this repository under the
// hostname the client used, hash fragments and metadata attributes intact.
// Files are exposed under `files/<scheme>/<host>/<path>`, a path that names
// the upstream location. Cache records add an opaque digest of the exact
// advertised query so distinct signed or selector URLs cannot share bytes.
// PEP 658 `.metadata` companions use the same mapping.
//
// Group repositories merge project pages across members (one entry per file
// name, first member wins) and merge the root index. Responses use the
// client's requested HTML or JSON representation. Artifact resolution checks
// the same filename owner and exact advertised target; member index failures
// prevent the group from serving an inconsistent partial view.
//
// Hosted repositories accept `twine upload` over the WireProtocol hook: the
// multipart upload POST and the simple-index GETs are claimed for hosted
// repositories only, while distribution downloads ride the generic asset
// pipeline. An uploaded file is stored at `packages/<project>/<filename>`
// (project normalized per PEP 503); the root index and project pages are
// synthesized per request from the stored files, in HTML or JSON per the
// client's Accept, with every file link absolute under the hostname the client
// used and a `#sha256=` fragment computed from the stored bytes.
//
// Coordinates are projected into the reserved "pypi" attribute namespace as
// pypi.name (normalized) and, for files, pypi.version parsed from the wheel
// or sdist file name. The repository takes no formatConfig. The package
// registers the "pypi" format on import and only depends on the public suxen
// SPI.
package pypi

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

func init() {
	format.Register(Format{})
}

// Format implements the Python simple index format.
type Format struct{}

var (
	_ format.RepositoryValidator     = Format{}
	_ format.UploadPolicy            = Format{}
	_ format.AttributeProjector      = Format{}
	_ format.ProxyPolicy             = Format{}
	_ format.IndexRewriter           = Format{}
	_ format.NegotiatedIndexRewriter = Format{}
	_ format.ProxyRequestResolver    = Format{}
	_ format.GroupMerger             = Format{}
	_ format.GroupSourceNormalizer   = Format{}
	_ format.WireProtocol            = Format{}
)

// Name returns the format identifier.
func (Format) Name() string { return "pypi" }

// ValidateUpload rejects generic PUTs: the native publication handler owns
// package payloads and metadata and enforces their publication invariants.
// Native publication uses WireTools directly and does not call this policy.
func (Format) ValidateUpload(_ format.Repository, _ string) error {
	return &format.PolicyViolation{
		Code:    "pypi_native_publish_required",
		Message: "hosted pypi repositories require twine upload; direct asset uploads are not supported",
	}
}

// ValidateRepository accepts hosted, proxy, and group repositories without
// formatConfig.
func (Format) ValidateRepository(repository format.Repository) error {
	for key := range repository.Config {
		return &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: fmt.Sprintf("pypi formatConfig does not support %q", key),
		}
	}
	return nil
}

// ProjectAttributes publishes the normalized project name and, for files,
// the version parsed from the file name.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	path := asset.Path
	if marker := strings.LastIndex(path, cacheIdentityMarker); marker >= 0 &&
		len(path)-marker == len(cacheIdentityMarker)+64 {
		path = path[:marker]
	}
	if marker := strings.LastIndex(path, cachePathMarker); marker >= 0 &&
		len(path)-marker == len(cachePathMarker)+64 {
		path = path[:marker]
	}
	info, ok := parsePath(path)
	if !ok || info.kind == kindRoot {
		return nil
	}
	if info.kind == kindProject {
		return map[string]any{"name": info.project}
	}
	name, version, ok := parseDistributionFilename(info.filename)
	if !ok {
		return nil
	}
	return map[string]any{"name": name, "version": version}
}

// MutableUpstreamPath marks the simple index pages as revalidated on the
// proxy TTL; distribution files (proxied or hosted) are immutable.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	return ok && (info.kind == kindRoot || info.kind == kindProject)
}

type pathKind uint8

const (
	kindRoot pathKind = iota + 1
	kindProject
	kindFile
	// kindHostedFile is a distribution file uploaded to a hosted repository,
	// stored at packages/<project>/<filename> rather than at a proxied
	// upstream location.
	kindHostedFile
)

// pathInfo is one parsed repository path.
type pathInfo struct {
	kind     pathKind
	project  string
	scheme   string
	host     string
	path     string // upstream path of a file, without leading slash
	filename string
}

const (
	simplePrefix   = "simple"
	filesPrefix    = "files/"
	packagesPrefix = "packages/"
)

// parsePath recognizes simple/, simple/<project>/, and
// files/<scheme>/<host>/<path>.
func parsePath(assetPath string) (pathInfo, bool) {
	assetPath = strings.Trim(assetPath, "/")
	if assetPath == simplePrefix {
		return pathInfo{kind: kindRoot}, true
	}
	if project, found := strings.CutPrefix(assetPath, simplePrefix+"/"); found {
		if !validProjectName(project) {
			return pathInfo{}, false
		}
		return pathInfo{kind: kindProject, project: project}, true
	}
	if rest, found := strings.CutPrefix(assetPath, packagesPrefix); found {
		project, filename, ok := strings.Cut(rest, "/")
		if !ok || !validProjectName(project) || !validFilename(filename) {
			return pathInfo{}, false
		}
		return pathInfo{kind: kindHostedFile, project: project, filename: filename, path: filename}, true
	}
	if rest, found := strings.CutPrefix(assetPath, filesPrefix); found {
		scheme, rest, ok := strings.Cut(rest, "/")
		if !ok || (scheme != "http" && scheme != "https") {
			return pathInfo{}, false
		}
		host, filePath, ok := strings.Cut(rest, "/")
		if !ok || host == "" || filePath == "" || strings.ContainsAny(host, "@?#") {
			return pathInfo{}, false
		}
		for _, segment := range strings.Split(filePath, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return pathInfo{}, false
			}
		}
		return pathInfo{
			kind:     kindFile,
			scheme:   scheme,
			host:     host,
			path:     filePath,
			filename: filePath[strings.LastIndex(filePath, "/")+1:],
		}, true
	}
	return pathInfo{}, false
}

var projectNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// validProjectName accepts PEP 503 normalized names and their unnormalized
// spellings (pip sends normalized names; other tools may not).
func validProjectName(name string) bool {
	return name != "" && len(name) <= 128 && projectNamePattern.MatchString(strings.ToLower(name))
}

// validFilename accepts a single distribution file name: non-empty, no path
// separators or dot segments, and no URL delimiters, so it cannot escape its
// project directory or the repository path it is spliced into.
func validFilename(filename string) bool {
	if filename == "" || filename == "." || filename == ".." {
		return false
	}
	return !strings.ContainsAny(filename, "/\\?#%") && !strings.ContainsAny(filename, " \t\r\n")
}

// normalizeProjectName applies PEP 503 normalization.
func normalizeProjectName(name string) string {
	if strings.IndexAny(name, "-_.") < 0 {
		return strings.ToLower(name)
	}
	var normalized strings.Builder
	normalized.Grow(len(name))
	separator := false
	for index := 0; index < len(name); index++ {
		switch name[index] {
		case '-', '_', '.':
			if !separator {
				normalized.WriteByte('-')
			}
			separator = true
		default:
			normalized.WriteByte(name[index])
			separator = false
		}
	}
	return strings.ToLower(normalized.String())
}

// filePath is the repository path a rewritten file link points at.
func filePath(scheme, host, upstreamPath string) string {
	segments := strings.Split(strings.TrimPrefix(upstreamPath, "/"), "/")
	for index, segment := range segments {
		// The upstream path is already escaped. Escape its percent signs once
		// more so net/http's URL.Path decoding preserves upstream escapes such
		// as %2F as distinct from literal path separators.
		segments[index] = url.PathEscape(segment)
	}
	return filesPrefix + scheme + "/" + host + "/" + strings.Join(segments, "/")
}

var sdistExtensions = []string{".tar.gz", ".zip", ".tar.bz2", ".tar.xz", ".tar"}

// parseDistributionFilename extracts the normalized project name and version
// from a wheel or sdist file name. ok is false for other files (.metadata,
// eggs, signatures).
func parseDistributionFilename(filename string) (name string, version string, ok bool) {
	if stem, found := strings.CutSuffix(filename, ".whl"); found {
		parts := strings.SplitN(stem, "-", 3)
		if len(parts) < 3 {
			return "", "", false
		}
		return normalizeProjectName(parts[0]), parts[1], true
	}
	for _, extension := range sdistExtensions {
		if stem, found := strings.CutSuffix(filename, extension); found {
			index := strings.LastIndex(stem, "-")
			if index <= 0 || index == len(stem)-1 {
				return "", "", false
			}
			return normalizeProjectName(stem[:index]), stem[index+1:], true
		}
	}
	return "", "", false
}
