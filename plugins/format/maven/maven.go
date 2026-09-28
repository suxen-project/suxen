// Package maven is the Maven repository format plugin.
//
// It teaches suxen the Maven repository layout: hosted repositories accept
// standard `mvn deploy` uploads (artifacts, checksums, signatures, and the
// client-maintained maven-metadata.xml indexes) and enforce a per-repository
// version policy; proxy repositories cache upstream artifacts immutably while
// revalidating the mutable paths — metadata indexes and non-unique SNAPSHOT
// artifacts — against the upstream on the proxy TTL. Artifact coordinates are
// projected into the reserved "maven" attribute namespace, so cleanup
// policies, download gates, and search can select on
// maven.groupId/artifactId/version like any other attribute.
//
// Index handling: group repositories merge maven-metadata.xml (and derive its
// checksums) across their members instead of serving the first match, and
// hosted repositories synthesize missing maven-metadata.xml from the stored
// artifacts — client-deployed metadata always wins, synthesized indexes only
// answer misses. GPG signatures of metadata cannot be synthesized or merged
// and resolve first-match.
//
// Repository formatConfig schema:
//
//	{"versionPolicy": "release" | "snapshot" | "mixed"}
//
// The default policy is "mixed". "release" rejects SNAPSHOT uploads and
// refuses SNAPSHOT paths on proxies; "snapshot" is the reverse. Hosted
// uploads are additionally validated against the repository layout; proxy
// paths outside the layout pass through, because upstreams legitimately
// serve indexes and key files the layout does not describe.
//
// The package registers the "maven" format on import and only depends on the
// public suxen SPI, so it doubles as the reference for out-of-tree format
// plugins.
package maven

import (
	"fmt"
	"path"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	policyRelease  = "release"
	policySnapshot = "snapshot"
	policyMixed    = "mixed"
)

func init() {
	format.Register(Format{})
}

// Format implements the Maven repository format.
type Format struct{}

// Name returns the format identifier.
func (Format) Name() string { return "maven" }

// RetentionGroupKey lets versions of one artifact compete for keepLast.
func (Format) RetentionGroupKey(_ format.Repository, asset format.Asset) (string, bool) {
	info, ok := parsePath(asset.Path)
	if !ok {
		return "", false
	}
	if info.Coordinates != nil {
		c := info.Coordinates
		return c.GroupID + "/" + c.ArtifactID, true
	}
	// Metadata paths can be both an artifact-level index for an artifact ID
	// ending in -SNAPSHOT and a version-level snapshot index. Without an
	// artifact anchor, keep them in their own directory for retention.
	return "", false
}

// CompanionPaths is empty: Maven checksums, signatures, classifiers, and
// version metadata are stored files handled by the directory retention unit.
func (Format) CompanionPaths(_ format.Repository, _ string) []string { return nil }

// RetentionUnitDirectory groups every direct child of a base-version
// directory. In particular, timestamped SNAPSHOT builds share one unit.
func (Format) RetentionUnitDirectory(_ format.Repository, assetPath string) string {
	info, ok := parsePath(assetPath)
	if !ok {
		return ""
	}
	if info.Coordinates == nil {
		// A metadata path under a -SNAPSHOT directory may be a version index
		// or an artifact index. It joins a unit only when an artifact anchors
		// that directory; otherwise ordinary cleanup handles it.
		if !info.Metadata || !info.SnapshotVersionDirectory || len(strings.Split(assetPath, "/")) < 4 {
			return ""
		}
	}
	return path.Dir(assetPath)
}

// IsRetentionUnitAnchor requires a real artifact file before a variable
// Maven version directory can enter cleanup. Metadata, checksums, and
// signatures cannot seed a unit, but remain part of an anchored directory.
func (Format) IsRetentionUnitAnchor(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	return ok && info.Coordinates != nil && !info.Checksum && !info.Signature
}

// ValidateRepository validates the formatConfig schema.
func (Format) ValidateRepository(repository format.Repository) error {
	for key := range repository.Config {
		if key != "versionPolicy" {
			return &format.PolicyViolation{
				Code:    "invalid_format_config",
				Message: fmt.Sprintf("maven formatConfig does not support %q", key),
			}
		}
	}
	if _, err := versionPolicy(repository); err != nil {
		return err
	}
	return nil
}

// ValidateUpload enforces the repository layout and the version policy on
// hosted uploads.
func (Format) ValidateUpload(repository format.Repository, assetPath string) error {
	info, ok := parsePath(assetPath)
	if !ok {
		return &format.PolicyViolation{
			Code: "maven_invalid_path",
			Message: fmt.Sprintf(
				"%q is not a Maven repository path (expected group/artifact/version/artifact-version[-classifier].ext or maven-metadata.xml)",
				assetPath,
			),
		}
	}
	policy, err := versionPolicy(repository)
	if err != nil {
		return err
	}
	switch policy {
	case policyRelease:
		if info.Coordinates != nil && info.SnapshotVersionDirectory {
			return &format.PolicyViolation{
				Code:    "maven_version_policy",
				Message: "repository accepts releases only; SNAPSHOT versions are rejected",
			}
		}
	case policySnapshot:
		if info.Coordinates != nil && !info.Coordinates.Snapshot {
			return &format.PolicyViolation{
				Code:    "maven_version_policy",
				Message: "repository accepts snapshots only; release versions are rejected",
			}
		}
	}
	return nil
}

// ProjectAttributes derives the maven coordinate namespace from the asset
// path.
func (Format) ProjectAttributes(asset format.Asset) map[string]any {
	info, ok := parsePath(asset.Path)
	if !ok {
		return nil
	}
	if info.Metadata {
		attributes := map[string]any{"metadata": true}
		markCompanion(attributes, info)
		return attributes
	}
	c := info.Coordinates
	attributes := map[string]any{
		"groupId":     c.GroupID,
		"artifactId":  c.ArtifactID,
		"version":     c.Version,
		"baseVersion": c.BaseVersion,
		"extension":   c.Extension,
		"snapshot":    c.Snapshot,
	}
	if c.Classifier != "" {
		attributes["classifier"] = c.Classifier
	}
	markCompanion(attributes, info)
	return attributes
}

func markCompanion(attributes map[string]any, info pathInfo) {
	if info.Checksum {
		attributes["checksum"] = true
	}
	if info.Signature {
		attributes["signature"] = true
	}
}

// MutableHostedPath permits client-maintained metadata indexes and their
// checksum/signature companions to change as new versions are published.
func (Format) MutableHostedPath(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	return ok && info.Metadata
}

// MutableUpstreamPath marks the upstream paths whose content changes over
// time: metadata indexes and non-unique SNAPSHOT artifacts (whose file names
// keep the SNAPSHOT literal instead of a deploy timestamp). Release artifacts
// and timestamped snapshot files are immutable and cached forever.
func (Format) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	info, ok := parsePath(assetPath)
	if !ok {
		return false
	}
	if info.Metadata {
		return true
	}
	return info.Coordinates.Snapshot &&
		info.Coordinates.Version == info.Coordinates.BaseVersion
}

func versionPolicy(repository format.Repository) (string, error) {
	value, present := repository.Config["versionPolicy"]
	if !present {
		return policyMixed, nil
	}
	policy, ok := value.(string)
	if !ok || (policy != policyRelease && policy != policySnapshot && policy != policyMixed) {
		return "", &format.PolicyViolation{
			Code:    "invalid_format_config",
			Message: "maven versionPolicy must be release, snapshot, or mixed",
		}
	}
	return policy, nil
}

var (
	_ format.Format                 = Format{}
	_ format.RepositoryValidator    = Format{}
	_ format.UploadPolicy           = Format{}
	_ format.AttributeProjector     = Format{}
	_ format.ProxyPolicy            = Format{}
	_ format.RetentionGrouping      = Format{}
	_ format.RetentionUnitDirectory = Format{}
	_ format.RetentionUnitAnchor    = Format{}
)
