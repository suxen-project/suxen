package pypi

import (
	"context"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

var _ format.GroupArtifactLocator = Format{}

// GroupArtifactCandidates finds the project and advertised filename using
// stored indexes. A PyPI file URL can be an arbitrary endpoint (including a
// signed query), so its basename does not reliably identify either one.
func (Format) GroupArtifactCandidates(ctx context.Context, repository format.Repository, request format.GroupArtifactRequest, stored format.StoredAssets) ([]format.GroupArtifactCandidate, bool, error) {
	info, ok := parsePath(request.Path)
	if !ok || (info.kind != kindFile && info.kind != kindHostedFile) {
		return nil, false, nil
	}
	if info.kind == kindHostedFile && repository.Type == "hosted" {
		found := false
		err := stored.VisitAssetPaths(ctx, request.Path, func(path string) (bool, error) {
			if path == request.Path {
				found = true
			}
			return false, nil
		})
		if err != nil {
			return nil, true, err
		}
		if !found {
			return nil, true, nil
		}
		filename := strings.TrimSuffix(info.filename, ".metadata")
		return []format.GroupArtifactCandidate{{SourcePath: simplePrefix + "/" + info.project, ArtifactKey: filename}}, true, nil
	}
	var candidates []format.GroupArtifactCandidate
	err := stored.VisitAssetPaths(ctx, simplePrefix+"/", func(sourcePath string) (bool, error) {
		sourceInfo, ok := parsePath(sourcePath)
		if !ok || sourceInfo.kind != kindProject {
			return true, nil
		}
		body, found, err := stored.ReadAsset(ctx, sourcePath)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		page, err := parsePage(body, kindProject)
		if err != nil {
			return true, nil // unrelated malformed indexes cannot identify this URL
		}
		base := pageDocumentURL(repository.Upstream, sourceInfo, page.baseHref)
		for _, entry := range page.entries {
			if groupEntryMatches(repository, base, entry, request) {
				filename, _ := entry["filename"].(string)
				candidates = append(candidates, format.GroupArtifactCandidate{SourcePath: sourcePath, ArtifactKey: filename})
			}
		}
		return true, nil
	})
	return candidates, true, err
}

func (Format) GroupSourceArtifact(repository format.Repository, sourcePath, artifactKey string, request format.GroupArtifactRequest, body []byte) (bool, bool, error) {
	info, ok := parsePath(sourcePath)
	if !ok || info.kind != kindProject {
		return false, false, nil
	}
	page, err := parsePage(body, kindProject)
	if err != nil {
		return false, false, err
	}
	if page.isQuarantined() {
		return false, false, &format.PolicyViolation{Code: "pypi_quarantined_project", Message: "PyPI project is quarantined"}
	}
	base := pageDocumentURL(repository.Upstream, info, page.baseHref)
	for _, entry := range page.entries {
		if entry["filename"] == artifactKey {
			return true, groupEntryMatches(repository, base, entry, request), nil
		}
	}
	return false, false, nil
}

func groupEntryMatches(repository format.Repository, base *url.URL, entry map[string]any, request format.GroupArtifactRequest) bool {
	publicPath := request.Path
	info, ok := parsePath(publicPath)
	if !ok {
		return false
	}
	link, _ := entry["url"].(string)
	if link == "" {
		return false
	}
	resolved, err := base.Parse(link)
	if err != nil {
		return false
	}
	resolved.Fragment = ""
	resolved.User = nil
	if info.kind == kindHostedFile && repository.Type != "proxy" {
		prefix := "/repository/" + repository.Name + "/"
		return strings.HasPrefix(resolved.Path, prefix) && strings.TrimPrefix(resolved.Path, prefix) == publicPath && request.RawQuery == "" && (request.StoredPath == "" || request.StoredPath == publicPath)
	}
	if info.kind != kindFile && info.kind != kindHostedFile {
		return false
	}
	requested, ok := requestedFileURL(repository, publicPath, request.RawQuery)
	if !ok {
		return false
	}
	if request.StoredPath != "" {
		if !isOpaqueGroupCachePath(request.StoredPath) {
			return false // legacy cache identity cannot establish an advertised generation
		}
		if sameAdvertisedTargetIgnoringQuery(resolved, requested) &&
			request.StoredPath == groupEntryCachePath(publicPath, resolved, entry, distributionEntry) {
			return true
		}
		if advertisesMetadata(entry) {
			companion := companionURL(resolved, ".metadata")
			if sameAdvertisedTargetIgnoringQuery(companion, requested) &&
				request.StoredPath == groupEntryCachePath(publicPath, companion, entry, metadataEntry) {
				return true
			}
		}
		if entry["gpg-sig"] == true {
			companion := companionURL(resolved, ".asc")
			return sameAdvertisedTargetIgnoringQuery(companion, requested) &&
				request.StoredPath == groupEntryCachePath(publicPath, companion, entry, signatureEntry)
		}
		return false
	}
	if sameAdvertisedTarget(resolved, requested) {
		return true
	}
	if advertisesMetadata(entry) && sameAdvertisedTarget(companionURL(resolved, ".metadata"), requested) {
		return true
	}
	return entry["gpg-sig"] == true && sameAdvertisedTarget(companionURL(resolved, ".asc"), requested)
}

func isOpaqueGroupCachePath(path string) bool {
	identity := strings.LastIndex(path, cacheIdentityMarker)
	if identity < 0 || !validCacheIdentity(path, path[:identity]+cacheIdentityMarker) {
		return false
	}
	query := strings.LastIndex(path[:identity], cachePathMarker)
	return query >= 0 && validCacheIdentity(path[:identity], path[:query]+cachePathMarker)
}

func sameAdvertisedTargetIgnoringQuery(candidate, requested *url.URL) bool {
	return candidate.Scheme == requested.Scheme && candidate.Host == requested.Host && candidate.EscapedPath() == requested.EscapedPath()
}

func groupEntryCachePath(publicPath string, target *url.URL, entry map[string]any, companion string) string {
	fragment := ""
	if link, ok := entry["url"].(string); ok {
		if parsed, err := url.Parse(link); err == nil {
			fragment = parsed.Fragment
		}
	}
	hashes, expected := entryIdentityMaterial(entry, fragment, companion)
	return proxyCachePath(publicPath, target.RawQuery) + cacheIdentityMarker + proxyAdvertisedIdentity(target.String(), hashes, expected)
}
