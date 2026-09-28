package gomod

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	listContentType = "text/plain; charset=utf-8"
	infoContentType = "application/json"
)

// versionInfo is the GOPROXY .info document.
type versionInfo struct {
	Version string `json:"Version"`
	Time    string `json:"Time,omitempty"`
}

// GroupMergeSource merges @v/list and @latest across members; module files
// resolve first-match.
func (Format) GroupMergeSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || (info.kind != kindList && info.kind != kindLatest) {
		return "", false
	}
	return assetPath, true
}

// MergeGroupContent unions version lists and selects the preferred member
// @latest. Malformed member content is skipped rather than failing the
// group, mirroring how `go` tolerates a proxy that lists junk.
func (Format) MergeGroupContent(
	_ format.Repository,
	assetPath string,
	sources [][]byte,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a mergeable go path", assetPath)
	}
	switch info.kind {
	case kindList:
		var versions []string
		for _, source := range sources {
			versions = append(versions, parseVersionList(source)...)
		}
		return renderVersionList(info.module, versions), listContentType, nil
	case kindLatest:
		var best []byte
		bestVersion := ""
		for _, source := range sources {
			var document versionInfo
			if err := json.Unmarshal(source, &document); err != nil || !validModuleVersion(info.module, document.Version) {
				continue
			}
			if bestVersion == "" || preferLatest(document.Version, bestVersion) {
				best, bestVersion = source, document.Version
			}
		}
		if best == nil {
			return nil, "", fmt.Errorf("no member of the group serves a valid %s", assetPath)
		}
		return best, infoContentType, nil
	default:
		return nil, "", fmt.Errorf("%q is not a mergeable go path", assetPath)
	}
}

// SynthesizeHosted derives @v/list and @latest of a hosted module from its
// complete sets of .info, .mod, and .zip files.
func (Format) SynthesizeHosted(
	ctx context.Context,
	_ format.Repository,
	assetPath string,
	assets format.StoredAssets,
) ([]byte, string, bool, error) {
	info, ok := parsePath(assetPath)
	if !ok || (info.kind != kindList && info.kind != kindLatest) {
		return nil, "", false, nil
	}
	escapedModule, err := module.EscapePath(info.module)
	if err != nil {
		return nil, "", false, nil
	}
	versions, err := storedVersions(ctx, info.module, escapedModule+"/@v/", assets)
	if err != nil {
		return nil, "", false, err
	}
	if len(versions) == 0 {
		return nil, "", false, nil
	}
	if info.kind == kindList {
		return renderVersionList(info.module, versions), listContentType, true, nil
	}

	latest := versions[0]
	for _, version := range versions[1:] {
		if preferLatest(version, latest) {
			latest = version
		}
	}
	escapedVersion, err := module.EscapeVersion(latest)
	if err != nil {
		return nil, "", false, nil
	}
	stored, found, err := assets.ReadAsset(ctx, escapedModule+"/@v/"+escapedVersion+".info")
	if err != nil {
		return nil, "", false, err
	}
	if found {
		return stored, infoContentType, true, nil
	}
	content, err := json.Marshal(versionInfo{Version: latest})
	if err != nil {
		return nil, "", false, err
	}
	return content, infoContentType, true, nil
}

// preferLatest follows GOPROXY's preference for releases, then prereleases,
// then pseudo-versions. Pseudo-versions compare by commit time, not by the
// semantic version of the tag from which their branch was derived.
func preferLatest(candidate, current string) bool {
	candidateRank, currentRank := latestRank(candidate), latestRank(current)
	if candidateRank != currentRank {
		return candidateRank > currentRank
	}
	if candidateRank == 0 {
		candidateTime, candidateErr := module.PseudoVersionTime(candidate)
		currentTime, currentErr := module.PseudoVersionTime(current)
		if candidateErr == nil && currentErr == nil && !candidateTime.Equal(currentTime) {
			return candidateTime.After(currentTime)
		}
	}
	return semver.Compare(candidate, current) > 0
}

func latestRank(version string) int {
	if module.IsPseudoVersion(version) {
		return 0
	}
	if semver.Prerelease(version) != "" {
		return 1
	}
	return 2
}

// storedVersions extracts the sorted, canonical versions with all three
// module files present. Uploads arrive separately, but an incomplete version
// cannot be downloaded by go and must not displace a usable @latest.
func storedVersions(ctx context.Context, modulePath, prefix string, assets format.StoredAssets) ([]string, error) {
	const (
		infoFile = 1 << iota
		modFile
		zipFile
	)
	seen := make(map[string]uint8)
	err := assets.VisitAssetPaths(ctx, prefix, func(assetPath string) (bool, error) {
		info, ok := parsePath(assetPath)
		if !ok || !info.kind.isModuleFile() || info.module != modulePath {
			return true, nil
		}
		if validModuleVersion(modulePath, info.version) {
			switch info.kind {
			case kindInfo:
				seen[info.version] |= infoFile
			case kindMod:
				seen[info.version] |= modFile
			case kindZip:
				seen[info.version] |= zipFile
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(seen))
	for version, files := range seen {
		if files == infoFile|modFile|zipFile {
			versions = append(versions, version)
		}
	}
	semver.Sort(versions)
	return versions, nil
}

func parseVersionList(content []byte) []string {
	var versions []string
	for _, line := range strings.Split(string(content), "\n") {
		if version := strings.TrimSpace(line); semver.IsValid(version) {
			versions = append(versions, version)
		}
	}
	return versions
}

// renderVersionList renders the GOPROXY list: canonical versions, sorted,
// without duplicates or pseudo-versions.
func renderVersionList(modulePath string, versions []string) []byte {
	seen := make(map[string]struct{})
	var listed []string
	for _, version := range versions {
		if module.IsPseudoVersion(version) || !validModuleVersion(modulePath, version) {
			continue
		}
		if _, duplicate := seen[version]; duplicate {
			continue
		}
		seen[version] = struct{}{}
		listed = append(listed, version)
	}
	sort.Slice(listed, func(i, j int) bool { return semver.Compare(listed[i], listed[j]) < 0 })
	if len(listed) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(listed, "\n") + "\n")
}
