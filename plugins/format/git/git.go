// Package git is the git snapshot repository format plugin.
//
// A git proxy repository serves upstream git repositories over the smart HTTP
// protocol (version 2) as snapshots: one self-contained depth-1 packfile per
// commit, plus a per-repository ref list. Serving never walks an object
// graph — a fetch is one ref lookup and one streamed blob — which is what
// keeps the format on the generic asset pipeline: the ref list is a mutable
// proxy asset revalidated on the proxy TTL, snapshot packs are immutable
// assets keyed by commit. On a miss the proxy performs a depth-1 fetch of the
// wanted commit from the upstream through the host's hardened outbound client
// and stores the upstream's pack verbatim.
//
// Every clone or fetch therefore yields a shallow clone of exactly one
// snapshot: history is never available, haves are ignored (the whole snapshot
// streams on every fetch), and a fetch may want a single commit — a plain
// `git clone` of a repository with several branches or tags is refused with a
// message pointing at `--depth 1`, `--single-branch`, or `--branch`. This
// covers CI checkouts (`actions/checkout`, `git clone --depth 1`, Docker build
// git contexts) without hosting a git server. Pushes are refused; hosted and
// group git repositories are not supported.
//
// Layout under a repository, for an upstream repository path `owner/name`
// (a trailing `.git` in the client's URL is accepted and ignored):
//
//	owner/name.git/refs                    ref list (mutable)
//	owner/name.git/snapshots/<commit>.pack  snapshot pack (immutable)
//
// The `git` attribute namespace publishes `git.repository` for both and
// `git.commit` for snapshots. The repository takes no formatConfig.
//
// The package registers the "git" format on import and only depends on the
// public suxen SPI.
package git

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
	endpointInfoRefs   = "info/refs"
)

func init() {
	format.Register(Format{})
}

// Format implements the git snapshot repository format.
type Format struct{}

var (
	_ format.RepositoryValidator = Format{}
	_ format.ProxyPolicy         = Format{}
	_ format.AttributeProjector  = Format{}
	_ format.WireProtocol        = Format{}
)

// Name returns the format identifier.
func (Format) Name() string { return "git" }

// ValidateRepository accepts proxy repositories without formatConfig.
func (Format) ValidateRepository(repository format.Repository) error {
	if repository.Type != "proxy" {
		return &format.PolicyViolation{
			Code:    "git_proxy_only",
			Message: "git repositories serve upstream snapshots and must be proxies",
		}
	}
	for key := range repository.Config {
		return &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: fmt.Sprintf("git formatConfig does not support %q", key),
		}
	}
	return nil
}

// MutableUpstreamPath marks the ref list as revalidated on the proxy TTL;
// snapshot packs are immutable.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	_, kind, _, ok := parseAssetPath(assetPath)
	return ok && kind == assetKindRefs
}

// ProjectAttributes publishes the upstream repository path and the known
// commit boundary of a snapshot. Annotated tags use their peeled commit;
// complete root snapshots have no shallow boundary to project.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	key, kind, objectID, ok := parseAssetPath(asset.Path)
	if !ok {
		return nil
	}
	attributes := map[string]any{"repository": key}
	if kind == assetKindSnapshot {
		boundary, known, err := snapshotBoundary(asset.ContentType)
		if err == nil && !known {
			// Snapshots stored before the boundary parameter used their path
			// object ID as the commit coordinate.
			attributes["commit"] = objectID
		} else if err == nil && boundary != "" {
			attributes["commit"] = boundary
		}
	}
	return attributes
}

// WireAction claims the smart HTTP endpoints. Fetching (the advertisement and
// upload-pack) is a read; anything addressing receive-pack is a write so an
// unauthorized push is refused before the format explains it is unsupported.
func (Format) WireAction(
	_ format.Repository,
	_ string,
	requestPath string,
	query url.Values,
) (string, bool) {
	route, ok := parseRoute(requestPath)
	if !ok {
		return "", false
	}
	switch route.endpoint {
	case endpointInfoRefs:
		if query.Get("service") == serviceReceivePack {
			return "write", true
		}
		return "read", true
	case serviceReceivePack:
		return "write", true
	default:
		return "read", true
	}
}

// ServeWire serves one smart HTTP request.
func (Format) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	requestPath string,
	tools format.WireTools,
) {
	route, ok := parseRoute(requestPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	server := protocolServer{tools: tools, repository: repository, route: route}
	switch route.endpoint {
	case endpointInfoRefs:
		server.serveInfoRefs(w, r)
	case serviceUploadPack:
		server.serveUploadPack(w, r)
	default:
		server.serveReceivePack(w, r)
	}
}

// route is one parsed smart HTTP request path.
type route struct {
	// repo is the upstream repository path exactly as the client spelled it
	// (with or without a trailing .git); it is forwarded to the upstream.
	repo string
	// key is repo without a trailing .git and names the assets.
	key string
	// endpoint is info/refs, git-upload-pack, or git-receive-pack.
	endpoint string
}

func parseRoute(requestPath string) (route, bool) {
	requestPath = strings.Trim(requestPath, "/")
	var repo, endpoint string
	switch {
	case strings.HasSuffix(requestPath, "/"+endpointInfoRefs):
		repo, endpoint = strings.TrimSuffix(requestPath, "/"+endpointInfoRefs), endpointInfoRefs
	case strings.HasSuffix(requestPath, "/"+serviceUploadPack):
		repo, endpoint = strings.TrimSuffix(requestPath, "/"+serviceUploadPack), serviceUploadPack
	case strings.HasSuffix(requestPath, "/"+serviceReceivePack):
		repo, endpoint = strings.TrimSuffix(requestPath, "/"+serviceReceivePack), serviceReceivePack
	default:
		return route{}, false
	}
	if !validRepoPath(repo) {
		return route{}, false
	}
	key := strings.TrimSuffix(repo, ".git")
	if key == "" || strings.HasSuffix(key, "/") {
		return route{}, false
	}
	return route{repo: repo, key: key, endpoint: endpoint}, true
}

// validRepoPath accepts slash-separated segments that are neither empty nor
// dot segments and carry no URL delimiters, so a repository path can never
// escape its asset prefix or rewrite the upstream request URL it is spliced
// into.
func validRepoPath(repo string) bool {
	if repo == "" || strings.ContainsAny(repo, "?#%\\ \t\r\n") {
		return false
	}
	for _, segment := range strings.Split(repo, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

const (
	assetKindRefs     = "refs"
	assetKindSnapshot = "snapshot"
)

func refsPath(key string) string {
	return key + ".git/refs"
}

func snapshotPath(key, commit string) string {
	return key + ".git/snapshots/" + commit + ".pack"
}

// parseAssetPath inverts refsPath and snapshotPath.
func parseAssetPath(assetPath string) (key string, kind string, commit string, ok bool) {
	if strings.HasSuffix(assetPath, ".git/refs") {
		key = strings.TrimSuffix(assetPath, ".git/refs")
		return key, assetKindRefs, "", validRepoPath(key)
	}
	prefix, file, found := strings.Cut(assetPath, ".git/snapshots/")
	if !found || !strings.HasSuffix(file, ".pack") {
		return "", "", "", false
	}
	commit = strings.TrimSuffix(file, ".pack")
	if !validObjectID(commit) || !validRepoPath(prefix) {
		return "", "", "", false
	}
	return prefix, assetKindSnapshot, commit, true
}

// validObjectID accepts a lowercase SHA-1 hex object id.
func validObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
