package maven

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	metadataContentType = "application/xml"
	checksumContentType = "text/plain"
)

// GroupMergeSource merges maven-metadata.xml across group members. Checksum
// companions are derived from the merged metadata, so their source is the
// metadata path itself; signatures cannot be synthesized and resolve
// first-match, as does archetype-catalog.xml (a different schema).
func (Format) GroupMergeSource(
	_ format.Repository,
	assetPath string,
) (string, bool) {
	basePath, _, ok := mavenMetadataTarget(assetPath)
	return basePath, ok
}

// MergeGroupContent merges the member metadata documents and renders either
// the merged XML or one of its checksums.
func (Format) MergeGroupContent(
	_ format.Repository,
	assetPath string,
	sources [][]byte,
) ([]byte, string, error) {
	_, checksumAlgorithm, ok := mavenMetadataTarget(assetPath)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a mergeable maven path", assetPath)
	}
	merged, err := mergeMetadata(sources)
	if err != nil {
		return nil, "", err
	}
	rendered, err := renderMetadata(merged)
	if err != nil {
		return nil, "", err
	}
	if checksumAlgorithm != "" {
		return checksumContent(rendered, checksumAlgorithm)
	}
	return rendered, metadataContentType, nil
}

// SynthesizeHosted answers reads of maven-metadata.xml (and its checksums)
// that have no stored asset by deriving the index from the artifacts that do
// exist. Client-deployed metadata always wins — the hook only runs on misses
// — so deleting a stale stored index is enough to switch a path to
// synthesized, always-consistent metadata.
func (Format) SynthesizeHosted(
	ctx context.Context,
	_ format.Repository,
	assetPath string,
	assets format.StoredAssets,
) ([]byte, string, bool, error) {
	basePath, checksumAlgorithm, ok := mavenMetadataTarget(assetPath)
	if !ok {
		return nil, "", false, nil
	}
	content, found, err := hostedMetadataContent(ctx, basePath, assets)
	if err != nil || !found {
		return nil, "", false, err
	}
	if checksumAlgorithm != "" {
		checksum, contentType, err := checksumContent(content, checksumAlgorithm)
		return checksum, contentType, err == nil, err
	}
	return content, metadataContentType, true, nil
}

// hostedMetadataContent returns the bytes a hosted metadata path serves: the
// stored asset when the client deployed one, otherwise an index synthesized
// from the sibling artifact files.
func hostedMetadataContent(
	ctx context.Context,
	basePath string,
	assets format.StoredAssets,
) ([]byte, bool, error) {
	stored, found, err := assets.ReadAsset(ctx, basePath)
	if err != nil || found {
		return stored, found, err
	}

	directory := strings.TrimSuffix(basePath, "maven-metadata.xml")
	if directory == basePath || directory == "" {
		return nil, false, nil
	}
	var enumerationErr error
	paths := func(yield func(string) bool) {
		enumerationErr = assets.VisitAssetPaths(ctx, directory, func(path string) (bool, error) {
			return yield(path), nil
		})
	}

	parent := strings.TrimSuffix(directory, "/")
	var document *metadataDocument
	var ok bool
	if strings.HasSuffix(parent, "-SNAPSHOT") {
		document, ok = synthesizeVersionMetadata(directory, paths)
		if enumerationErr != nil {
			return nil, false, enumerationErr
		}
		if !ok {
			// The same path can be an artifact-level index when the artifact ID
			// itself ends in -SNAPSHOT. Nested version artifacts identify it.
			document, ok = synthesizeArtifactMetadata(directory, paths)
		}
	} else {
		document, ok = synthesizeArtifactMetadata(directory, paths)
	}
	if enumerationErr != nil {
		return nil, false, enumerationErr
	}
	if !ok {
		return nil, false, nil
	}
	content, err := renderMetadata(document)
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// mavenMetadataTarget classifies a path as maven-metadata.xml or one of its
// checksum companions, returning the metadata path the content derives from.
func mavenMetadataTarget(assetPath string) (basePath string, checksumAlgorithm string, ok bool) {
	basePath = assetPath
	for _, extension := range checksumExtensions {
		if strings.HasSuffix(basePath, extension) {
			checksumAlgorithm = strings.TrimPrefix(extension, ".")
			basePath = strings.TrimSuffix(basePath, extension)
			break
		}
	}
	if basePath != "maven-metadata.xml" &&
		!strings.HasSuffix(basePath, "/maven-metadata.xml") {
		return "", "", false
	}
	return basePath, checksumAlgorithm, true
}

func checksumContent(content []byte, algorithm string) ([]byte, string, error) {
	var digest []byte
	switch algorithm {
	case "md5":
		sum := md5.Sum(content)
		digest = sum[:]
	case "sha1":
		sum := sha1.Sum(content)
		digest = sum[:]
	case "sha256":
		sum := sha256.Sum256(content)
		digest = sum[:]
	case "sha512":
		sum := sha512.Sum512(content)
		digest = sum[:]
	default:
		return nil, "", errors.New("unsupported checksum algorithm " + algorithm)
	}
	return []byte(hex.EncodeToString(digest)), checksumContentType, nil
}

// ValidateProxyPath enforces the version policy on proxy repositories: a
// release proxy never serves SNAPSHOT paths and a snapshot proxy never serves
// release artifacts. Paths outside the Maven layout pass through — upstreams
// legitimately serve indexes and key files the layout does not describe.
func (Format) ValidateProxyPath(
	repository format.Repository,
	assetPath string,
) error {
	policy, err := versionPolicy(repository)
	if err != nil {
		return err
	}
	if policy == policyMixed {
		return nil
	}
	info, ok := parsePath(assetPath)
	if !ok {
		return nil
	}
	switch policy {
	case policyRelease:
		// Metadata under a -SNAPSHOT directory can also be the index of an
		// artifact whose ID ends in -SNAPSHOT. A path-only policy cannot
		// distinguish those cases; artifact filenames remain enforced.
		if info.Coordinates != nil && info.SnapshotVersionDirectory {
			return &format.PolicyViolation{
				Code:    "maven_version_policy",
				Message: "repository serves releases only; SNAPSHOT paths are rejected",
			}
		}
	case policySnapshot:
		if info.Coordinates != nil && !info.Coordinates.Snapshot {
			return &format.PolicyViolation{
				Code:    "maven_version_policy",
				Message: "repository serves snapshots only; release paths are rejected",
			}
		}
	}
	return nil
}

var (
	_ format.GroupMerger       = Format{}
	_ format.HostedSynthesizer = Format{}
	_ format.ProxyPathPolicy   = Format{}
)
