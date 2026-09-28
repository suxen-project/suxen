package npm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/suxen-project/suxen/spi/format"
)

const packumentContentType = "application/json"

const proxyIdentityMarker = ".suxen-source-sha256-"

// RewriteIndex points every dist.tarball of a packument at this repository.
// The local path identifies the package version; the original URL remains in
// the cached upstream document for the eventual fetch (see ResolveProxyRequest).
func (Format) RewriteIndex(
	_ format.Repository,
	assetPath string,
	body []byte,
	contentType string,
	repositoryURL string,
) ([]byte, string, error) {
	return rewriteIndexWithLimit(assetPath, body, contentType, repositoryURL, packumentRenderedLimit)
}

func rewriteIndexWithLimit(assetPath string, body []byte, contentType, repositoryURL string, limit int64) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindPackument {
		return body, contentType, nil
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, "", fmt.Errorf("upstream packument: %w", err)
	}
	replacement := func(version, original string) string {
		target, err := url.Parse(original)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return original
		}
		return repositoryURL + "/" + tarballPath(info.name, tarballFile(info.name, version))
	}
	projected, err := jsonEncodedSize(document, math.MaxInt64)
	if err != nil {
		return nil, "", err
	}
	// A request hostname is repeated for every version's tarball URL. Project
	// all removals before additions so a valid output cannot fail according to
	// map iteration order when some URLs shrink and others grow.
	var budgetError error
	rewriteTarballs(document, func(version, original string) string {
		if budgetError != nil {
			return original
		}
		newURL := replacement(version, original)
		if newURL == original {
			return original
		}
		oldSize, err := jsonEncodedSize(original, math.MaxInt64)
		if err != nil {
			budgetError = err
			return original
		}
		projected -= oldSize
		return original
	})
	if budgetError != nil {
		return nil, "", budgetError
	}
	if projected > limit {
		return nil, "", errPackumentBudget
	}
	rewriteTarballs(document, func(version, original string) string {
		if budgetError != nil {
			return original
		}
		newURL := replacement(version, original)
		if newURL == original {
			return original
		}
		newSize, err := jsonEncodedSize(newURL, limit)
		if err != nil {
			budgetError = err
			return original
		}
		if newSize > limit-projected {
			budgetError = errPackumentBudget
			return original
		}
		projected += newSize
		return original
	})
	if budgetError != nil {
		return nil, "", budgetError
	}
	rewriteTarballs(document, replacement)
	rewritten, err := json.Marshal(document)
	if err != nil {
		return nil, "", err
	}
	return rewritten, packumentContentType, nil
}

// rewriteTarballs applies replace to every versions[*].dist.tarball string.
func rewriteTarballs(document map[string]any, replace func(string, string) string) {
	versions, _ := document["versions"].(map[string]any)
	for versionName, entry := range versions {
		version, _ := entry.(map[string]any)
		dist, _ := version["dist"].(map[string]any)
		tarball, _ := dist["tarball"].(string)
		if tarball != "" {
			if rewritten := replace(versionName, tarball); rewritten != tarball {
				dist["tarball"] = rewritten
			}
		}
	}
}

// ResolveProxyRequest fetches a tarball from the URL the cached packument advertises
// for that file, so registries whose tarballs live on another host than the
// metadata are proxied whole. When a version disappears from the packument,
// or the packument itself is deleted, a single unambiguous cached tarball
// remains readable.
func (Format) ResolveProxyRequest(
	ctx context.Context,
	_ format.Repository,
	assetPath string,
	_ string,
	stored format.StoredAssets,
) (format.ResolvedProxyRequest, error) {
	resolved := format.ResolvedProxyRequest{CachePath: assetPath}
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindTarball {
		return resolved, nil
	}
	content, found, err := stored.ReadAsset(ctx, info.name)
	if err != nil {
		return resolved, err
	}
	if !found {
		return recoverNpmProxyTarball(ctx, stored, resolved, false)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball   string          `json:"tarball"`
				Integrity json.RawMessage `json:"integrity"`
				Shasum    json.RawMessage `json:"shasum"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return resolved, nil
	}
	if version, found := document.Versions[info.version]; found {
		expected, err := advertisedDigests(version.Dist.Integrity, version.Dist.Shasum)
		if err != nil {
			return format.ResolvedProxyRequest{}, fmt.Errorf("cached packument integrity for %s@%s: %w", info.name, info.version, err)
		}
		resolved.ExpectedDigests = expected
		tarball := version.Dist.Tarball
		if parsed, err := url.Parse(tarball); err == nil &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil {
			resolved.UpstreamURL = tarball
		}
		if resolved.UpstreamURL != "" || len(expected) != 0 {
			identity := sha256.Sum256([]byte(resolved.UpstreamURL + "\n" + strings.Join(expected, "\n")))
			resolved.CachePath += proxyIdentityMarker + hex.EncodeToString(identity[:])
		}
	} else {
		// A removed version may still have an immutable cached tarball. Its
		// original URL is no longer known, so never fetch the public path from
		// the configured upstream if the cache entry vanishes or is ambiguous.
		return recoverNpmProxyTarball(ctx, stored, resolved, true)
	}
	return resolved, nil
}

func recoverNpmProxyTarball(
	ctx context.Context,
	stored format.StoredAssets,
	resolved format.ResolvedProxyRequest,
	missingVersion bool,
) (format.ResolvedProxyRequest, error) {
	// Multiple historical identities mean the advertised URL or digest
	// changed; selecting either one could return the wrong tarball.
	prefix := resolved.CachePath + proxyIdentityMarker
	var candidate string
	ambiguous := false
	err := stored.VisitAssetPaths(ctx, prefix, func(path string) (bool, error) {
		if !isNpmProxyIdentity(path, prefix) {
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
		return resolved, err
	}
	if candidate != "" && !ambiguous {
		resolved.CachePath = candidate
		// A concurrent deletion must not fall through to another target.
		resolved.CacheOnly = true
	} else if missingVersion {
		resolved.CacheOnly = true
	}
	return resolved, nil
}

func isNpmProxyIdentity(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) || len(path) != len(prefix)+sha256.Size*2 {
		return false
	}
	for _, char := range path[len(prefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// advertisedDigests chooses the strongest supported SRI algorithm. Tokens of
// that algorithm are alternatives; weaker tokens cannot override a mismatch.
// A legacy shasum applies only when no SRI integrity was advertised.
func advertisedDigests(integrityRaw, shasumRaw json.RawMessage) ([]string, error) {
	var integrity string
	if len(integrityRaw) != 0 && string(integrityRaw) != "null" {
		if err := json.Unmarshal(integrityRaw, &integrity); err != nil {
			return nil, fmt.Errorf("invalid dist.integrity: %w", err)
		}
	}
	if integrity != "" {
		strongest := 0
		for _, token := range strings.Fields(integrity) {
			algorithm, _, found := strings.Cut(token, "-")
			if found && integrityStrength(algorithm) > strongest {
				strongest = integrityStrength(algorithm)
			}
		}
		if strongest == 0 {
			return nil, errors.New("dist.integrity has no supported hash algorithm")
		}
		var expected []string
		for _, token := range strings.Fields(integrity) {
			algorithm, value, found := strings.Cut(token, "-")
			if !found || integrityStrength(algorithm) != strongest {
				continue
			}
			value, _, _ = strings.Cut(value, "?")
			digest, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				digest, err = base64.RawStdEncoding.DecodeString(value)
			}
			if err != nil || len(digest) != integrityDigestSize(algorithm) {
				return nil, fmt.Errorf("invalid %s dist.integrity token", algorithm)
			}
			expected = append(expected, algorithm+":"+hex.EncodeToString(digest))
		}
		slices.Sort(expected)
		expected = slices.Compact(expected)
		return expected, nil
	}
	if len(shasumRaw) == 0 || string(shasumRaw) == "null" {
		return nil, nil
	}
	var shasum string
	if err := json.Unmarshal(shasumRaw, &shasum); err != nil {
		return nil, fmt.Errorf("invalid dist.shasum: %w", err)
	}
	if shasum == "" {
		return nil, nil
	}
	digest, err := hex.DecodeString(shasum)
	if err != nil || len(digest) != 20 {
		return nil, errors.New("invalid dist.shasum")
	}
	return []string{"sha1:" + hex.EncodeToString(digest)}, nil
}

func integrityStrength(algorithm string) int {
	switch algorithm {
	case "sha1":
		return 1
	case "sha256":
		return 2
	case "sha384":
		return 3
	case "sha512":
		return 4
	default:
		return 0
	}
}

func integrityDigestSize(algorithm string) int {
	switch algorithm {
	case "sha1":
		return 20
	case "sha256":
		return 32
	case "sha384":
		return 48
	case "sha512":
		return 64
	default:
		return 0
	}
}

// GroupMergeSource merges packuments across members; tarballs use
// GroupArtifactSelector to choose the advertising member.
func (Format) GroupMergeSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindPackument {
		return "", false
	}
	return assetPath, true
}

// MergeGroupContent unions member packuments: versions and time entries
// first-member-wins per key, `latest` recomputed as the highest release
// across members, everything else from the first member that has it.
// Every source must be a valid packument so artifact ownership agrees with
// the versions exposed by the merged index.
func (Format) MergeGroupContent(
	_ format.Repository,
	assetPath string,
	sources [][]byte,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindPackument {
		return nil, "", fmt.Errorf("%q is not a mergeable npm path", assetPath)
	}
	var merged map[string]any
	for index, source := range sources {
		var document map[string]any
		if err := json.Unmarshal(source, &document); err != nil {
			return nil, "", fmt.Errorf("invalid npm packument from member %d: %w", index+1, err)
		}
		if document == nil {
			return nil, "", fmt.Errorf("invalid npm packument from member %d: expected object", index+1)
		}
		if _, ok := document["versions"].(map[string]any); !ok {
			return nil, "", fmt.Errorf("invalid npm packument from member %d: missing versions object", index+1)
		}
		if merged == nil {
			merged = document
			ensureMap(merged, "versions")
			ensureMap(merged, "dist-tags")
			continue
		}
		mergeMap(merged, document, "versions")
		mergeMap(merged, document, "time")
		mergeMap(merged, document, "dist-tags")
		for key, value := range document {
			if _, present := merged[key]; !present {
				merged[key] = value
			}
		}
	}
	if merged == nil {
		return nil, "", errors.New("no member of the group serves a valid packument")
	}
	if latest := highestRelease(merged["versions"].(map[string]any)); latest != "" {
		merged["dist-tags"].(map[string]any)["latest"] = latest
	}
	if _, err := jsonEncodedSize(merged, packumentRenderedLimit); err != nil {
		return nil, "", err
	}
	content, err := json.Marshal(merged)
	if err != nil {
		return nil, "", err
	}
	return content, packumentContentType, nil
}

func ensureMap(document map[string]any, key string) {
	if _, ok := document[key].(map[string]any); !ok {
		document[key] = map[string]any{}
	}
}

// mergeMap copies entries of source[key] that target[key] lacks.
func mergeMap(target, source map[string]any, key string) {
	from, _ := source[key].(map[string]any)
	if len(from) == 0 {
		return
	}
	into, ok := target[key].(map[string]any)
	if !ok {
		into = map[string]any{}
		target[key] = into
	}
	for entry, value := range from {
		if _, present := into[entry]; !present {
			into[entry] = value
		}
	}
}

// highestRelease returns the highest non-prerelease version among the keys,
// or the highest version overall when every version is a prerelease.
func highestRelease(versions map[string]any) string {
	best, bestPrerelease := "", ""
	for version := range versions {
		if !validNpmVersion(version) {
			continue
		}
		canonical := "v" + version
		if semver.Prerelease(canonical) != "" {
			if bestPrerelease == "" || compareNpmRelease(version, bestPrerelease) > 0 {
				bestPrerelease = version
			}
			continue
		}
		if best == "" || compareNpmRelease(version, best) > 0 {
			best = version
		}
	}
	if best == "" {
		return bestPrerelease
	}
	return best
}

// npm's semver parser accepts at most 256 characters and represents each
// core number as a JavaScript safe integer. Check those bounds in addition to
// the SemVer syntax shared with the Go parser.
func validNpmVersion(version string) bool {
	if len(version) == 0 || len(version) > 256 {
		return false
	}
	base, _, _ := strings.Cut(version, "+")
	if !semver.IsValid("v"+version) || semver.Canonical("v"+version) != "v"+base {
		return false
	}
	core, _, _ := strings.Cut(base, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	const maxSafeInteger = 9007199254740991
	for _, part := range parts {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil || number > maxSafeInteger {
			return false
		}
	}
	return true
}

// Build metadata does not affect SemVer precedence. Break ties by the full
// published spelling so latest remains stable across requests.
func compareNpmRelease(left, right string) int {
	if comparison := semver.Compare("v"+left, "v"+right); comparison != 0 {
		return comparison
	}
	return strings.Compare(left, right)
}
