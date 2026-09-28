package cargo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	configContentType = "application/json"
	indexContentType  = "text/plain; charset=utf-8"
	// downloadTemplate is the dl entry this repository advertises; cargo
	// substitutes the markers itself.
	downloadTemplate    = "/dl/{crate}/{version}/download"
	proxyIdentityMarker = ".suxen-source-sha256-"
)

// RewriteIndex points the registry config at this repository: `dl` becomes
// the repository's download template and `api` is dropped because the
// registry API is not proxied. Index files carry no URLs and pass through.
func (Format) RewriteIndex(
	_ format.Repository,
	assetPath string,
	body []byte,
	contentType string,
	repositoryURL string,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindConfig {
		return body, contentType, nil
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, "", fmt.Errorf("upstream config.json: %w", err)
	}
	if config == nil {
		return nil, "", errors.New("upstream config.json is not an object")
	}
	config["dl"] = repositoryURL + downloadTemplate
	delete(config, "api")
	// Cargo otherwise omits its token from crate downloads, which breaks
	// private repositories after an authenticated sparse-index lookup.
	config["auth-required"] = true
	rewritten, err := json.Marshal(config)
	if err != nil {
		return nil, "", err
	}
	return rewritten, configContentType, nil
}

// ResolveProxyRequest resolves a crate download through the `dl` template of the
// cached upstream config.json, which cargo fetches before asking for a crate.
// A crate already in the immutable cache remains readable if config.json is
// later deleted. Every other path is fetched from the upstream index as-is.
func (Format) ResolveProxyRequest(
	ctx context.Context,
	_ format.Repository,
	assetPath string,
	_ string,
	stored format.StoredAssets,
) (format.ResolvedProxyRequest, error) {
	resolved := format.ResolvedProxyRequest{CachePath: assetPath}
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindCrate {
		return resolved, nil
	}
	content, found, err := stored.ReadAsset(ctx, configPath)
	if err != nil {
		return format.ResolvedProxyRequest{}, err
	}
	if !found {
		retained, cached, err := recoverCargoProxyCrate(ctx, stored, resolved)
		if err != nil {
			return format.ResolvedProxyRequest{}, err
		}
		if !cached && retained.CachePath == assetPath {
			return format.ResolvedProxyRequest{}, errors.New("registry config.json is not cached yet; fetch config.json from this repository first")
		}
		return retained, nil
	}
	var config struct {
		Download string `json:"dl"`
	}
	if err := json.Unmarshal(content, &config); err != nil {
		return format.ResolvedProxyRequest{}, fmt.Errorf("cached config.json: %w", err)
	}
	if config.Download == "" {
		return format.ResolvedProxyRequest{}, errors.New("cached config.json has no dl entry")
	}
	upstreamURL, err := expandDownloadTemplate(config.Download, info.name, info.version)
	if err != nil {
		return format.ResolvedProxyRequest{}, err
	}
	resolved.UpstreamURL = upstreamURL
	index, found, err := stored.ReadAsset(ctx, indexPath(info.name))
	if err != nil {
		return format.ResolvedProxyRequest{}, err
	}
	if !found {
		retained, _, err := recoverCargoProxyCrate(ctx, stored, resolved)
		return retained, err
	}
	checksum, found, err := indexChecksum(index, info.name, info.version)
	if err != nil {
		return format.ResolvedProxyRequest{}, fmt.Errorf("cached index for %s %s: %w", info.name, info.version, err)
	}
	if !found {
		retained, _, err := recoverCargoProxyCrate(ctx, stored, resolved)
		return retained, err
	}
	resolved.ExpectedDigests = []string{"sha256:" + checksum}
	identity := sha256.Sum256([]byte(upstreamURL + "\n" + resolved.ExpectedDigests[0]))
	resolved.CachePath += proxyIdentityMarker + hex.EncodeToString(identity[:])
	return resolved, nil
}

// A removed index entry or config can only identify one retained crate if its
// historical cache identity is unambiguous. Never fetch an unadvertised crate.
func recoverCargoProxyCrate(ctx context.Context, stored format.StoredAssets, resolved format.ResolvedProxyRequest) (format.ResolvedProxyRequest, bool, error) {
	prefix := resolved.CachePath + proxyIdentityMarker
	var candidate string
	ambiguous := false
	err := stored.VisitAssetPaths(ctx, resolved.CachePath, func(path string) (bool, error) {
		if path != resolved.CachePath && !isCargoProxyIdentity(path, prefix) {
			return true, nil
		}
		if candidate != "" {
			ambiguous = true
			return false, nil
		}
		candidate = path
		return true, nil
	})
	if err != nil {
		return format.ResolvedProxyRequest{}, false, err
	}
	if candidate != "" && !ambiguous {
		resolved.CachePath = candidate
	} else if ambiguous {
		// The public path may itself be one historical cache entry. Keep
		// ambiguity on a path that cannot identify any valid cache entry.
		resolved.CachePath = prefix + "ambiguous"
	}
	resolved.CacheOnly = true
	return resolved, candidate != "" && !ambiguous, nil
}

func isCargoProxyIdentity(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) || len(path) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(path[len(prefix):])
	return err == nil
}

// indexChecksum requires the exact advertised spelling for a download URL.
// Publishing and group index merging still use build-agnostic versionIdentity
// to prevent duplicate versions that differ only in build metadata.
func indexChecksum(body []byte, name, version string) (string, bool, error) {
	advertisedVersion, checksum, found, err := indexVersionChecksum(body, name, version)
	if err != nil || !found || advertisedVersion != version {
		return "", false, err
	}
	return checksum, true, nil
}

// indexVersionChecksum selects the first entry for a canonical version, just
// as group index merging does. The caller decides whether the exact published
// spelling is also required for a download.
func indexVersionChecksum(body []byte, name, version string) (advertisedVersion, checksum string, found bool, err error) {
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Name     string `json:"name"`
			Version  string `json:"vers"`
			Checksum string `json:"cksum"`
		}
		if err := json.Unmarshal(line, &entry); err != nil || !validVersion(entry.Version) {
			continue
		}
		if versionIdentity(entry.Version) != versionIdentity(version) {
			continue
		}
		if !strings.EqualFold(entry.Name, name) {
			return "", "", false, errors.New("index entry crate name differs from request")
		}
		decoded, err := hex.DecodeString(entry.Checksum)
		if err != nil || len(decoded) != sha256.Size {
			return "", "", false, errors.New("index entry has invalid sha256 checksum")
		}
		return entry.Version, hex.EncodeToString(decoded), true, nil
	}
	return "", "", false, nil
}

// expandDownloadTemplate applies the registry `dl` template rules: markers are
// substituted when present, otherwise /{crate}/{version}/download is appended.
func expandDownloadTemplate(template, name, version string) (string, error) {
	if strings.Contains(template, "{sha256-checksum}") {
		return "", errors.New("upstream dl template uses {sha256-checksum}, which is not supported")
	}
	markers := []string{"{crate}", "{version}", "{prefix}", "{lowerprefix}"}
	hasMarker := false
	for _, marker := range markers {
		if strings.Contains(template, marker) {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		return strings.TrimSuffix(template, "/") + "/" + name + "/" + version + "/download", nil
	}
	prefix := strings.TrimSuffix(indexPath(name), "/"+strings.ToLower(name))
	replacer := strings.NewReplacer(
		"{crate}", name,
		"{version}", version,
		"{prefix}", prefixWithCase(prefix, name),
		"{lowerprefix}", prefix,
	)
	return replacer.Replace(template), nil
}

// prefixWithCase rebuilds the index prefix with the crate name's own case,
// which is what {prefix} means as opposed to {lowerprefix}.
func prefixWithCase(lowerPrefix, name string) string {
	switch len(name) {
	case 1, 2:
		return lowerPrefix
	case 3:
		return "3/" + name[:1]
	default:
		return name[:2] + "/" + name[2:4]
	}
}

// GroupMergeSource merges config.json and index files across members; crate
// files resolve first-match.
func (Format) GroupMergeSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind == kindCrate {
		return "", false
	}
	return assetPath, true
}

// MergeGroupContent unions index entries by version (first member wins) and
// takes the first parseable member config, which the IndexRewriter then
// points at the group.
func (Format) MergeGroupContent(
	_ format.Repository,
	assetPath string,
	sources [][]byte,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a mergeable cargo path", assetPath)
	}
	switch info.kind {
	case kindConfig:
		for _, source := range sources {
			var config map[string]any
			if json.Unmarshal(source, &config) == nil && config != nil {
				return source, configContentType, nil
			}
		}
		return nil, "", errors.New("no member of the group serves a valid config.json")
	case kindIndex:
		merged, err := mergeIndexEntries(info.name, sources)
		return merged, indexContentType, err
	default:
		return nil, "", fmt.Errorf("%q is not a mergeable cargo path", assetPath)
	}
}

// mergeIndexEntries keeps one JSON line per version, in member order, and
// drops lines that are not index entries.
func mergeIndexEntries(name string, sources [][]byte) ([]byte, error) {
	seen := make(map[string]struct{})
	var merged bytes.Buffer
	for _, source := range sources {
		for _, line := range bytes.Split(source, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var entry struct {
				Name     string `json:"name"`
				Version  string `json:"vers"`
				Checksum string `json:"cksum"`
			}
			if err := json.Unmarshal(line, &entry); err != nil || !validVersion(entry.Version) {
				continue
			}
			checksum, err := hex.DecodeString(entry.Checksum)
			if !strings.EqualFold(entry.Name, name) || err != nil || len(checksum) != sha256.Size {
				return nil, errors.New("group index entry has invalid name or sha256 checksum")
			}
			identity := versionIdentity(entry.Version)
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			merged.Write(line)
			merged.WriteByte('\n')
		}
	}
	return merged.Bytes(), nil
}
