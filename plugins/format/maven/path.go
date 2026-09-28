package maven

import (
	"regexp"
	"strings"
)

// coordinates identify one artifact file within the Maven repository layout
// {group/path}/{artifactId}/{baseVersion}/{artifactId}-{version}[-{classifier}].{extension}.
type coordinates struct {
	GroupID    string
	ArtifactID string
	// Version is the file version: for unique snapshots the timestamped form
	// (1.0-20260807.120000-1), otherwise equal to BaseVersion.
	Version string
	// BaseVersion is the version directory (1.0 or 1.0-SNAPSHOT).
	BaseVersion string
	Classifier  string
	Extension   string
	Snapshot    bool
}

// pathInfo classifies one repository-relative path.
type pathInfo struct {
	// Metadata marks repository metadata indexes (maven-metadata.xml,
	// archetype-catalog.xml) at any directory level.
	Metadata bool
	// Checksum marks a .md5/.sha1/.sha256/.sha512 companion of the base file.
	Checksum bool
	// Signature marks a .asc companion of the base file.
	Signature bool
	// Coordinates are set for artifact files (including their checksum and
	// signature companions) and nil for metadata files.
	Coordinates *coordinates
	// SnapshotVersionDirectory reports that the path sits under a
	// *-SNAPSHOT version directory; also set for version-level metadata.
	SnapshotVersionDirectory bool
}

var checksumExtensions = []string{".md5", ".sha1", ".sha256", ".sha512"}

// uniqueSnapshotPattern matches the timestamp-build suffix of unique snapshot
// versions: 20260807.120000-1.
var uniqueSnapshotPattern = regexp.MustCompile(`^\d{8}\.\d{6}-\d+$`)

// parsePath classifies a cleaned repository-relative path against the Maven
// repository layout. It returns false for paths that belong to no Maven
// repository (wrong directory depth, filename not derived from the
// coordinates, malformed snapshot timestamps).
func parsePath(assetPath string) (pathInfo, bool) {
	if assetPath == "" || strings.HasPrefix(assetPath, "/") {
		return pathInfo{}, false
	}
	segments := strings.Split(assetPath, "/")
	fileName := segments[len(segments)-1]

	info := pathInfo{}
	for _, extension := range checksumExtensions {
		if strings.HasSuffix(fileName, extension) {
			info.Checksum = true
			fileName = strings.TrimSuffix(fileName, extension)
			break
		}
	}
	if strings.HasSuffix(fileName, ".asc") {
		info.Signature = true
		fileName = strings.TrimSuffix(fileName, ".asc")
	}
	if fileName == "" {
		return pathInfo{}, false
	}

	if fileName == "maven-metadata.xml" || fileName == "archetype-catalog.xml" {
		info.Metadata = true
		if len(segments) >= 2 {
			parent := segments[len(segments)-2]
			info.SnapshotVersionDirectory = strings.HasSuffix(parent, "-SNAPSHOT")
		}
		return info, true
	}

	// Artifact files need group (>= 1 segment), artifactId, version, file.
	if len(segments) < 4 {
		return pathInfo{}, false
	}
	baseVersion := segments[len(segments)-2]
	artifactID := segments[len(segments)-3]
	groupID := strings.Join(segments[:len(segments)-3], ".")
	if artifactID == "" || baseVersion == "" || groupID == "" {
		return pathInfo{}, false
	}

	parsed, ok := parseArtifactFileName(fileName, artifactID, baseVersion)
	if !ok {
		return pathInfo{}, false
	}
	parsed.GroupID = groupID
	info.Coordinates = &parsed
	info.SnapshotVersionDirectory = parsed.Snapshot
	return info, true
}

// parseArtifactFileName validates {artifactId}-{version}[-{classifier}].{extension}
// where version is the base version, or its timestamped form under a
// *-SNAPSHOT base version.
func parseArtifactFileName(
	fileName string,
	artifactID string,
	baseVersion string,
) (coordinates, bool) {
	rest, found := strings.CutPrefix(fileName, artifactID+"-")
	if !found || rest == "" {
		return coordinates{}, false
	}

	snapshot := strings.HasSuffix(baseVersion, "-SNAPSHOT")
	version := ""
	switch {
	case strings.HasPrefix(rest, baseVersion):
		version = baseVersion
	case snapshot:
		// Unique snapshot: {yyyymmdd.hhmmss}-{build} replaces the SNAPSHOT
		// literal in the file name.
		prefix := strings.TrimSuffix(baseVersion, "SNAPSHOT")
		timestamped, found := strings.CutPrefix(rest, prefix)
		if !found {
			return coordinates{}, false
		}
		tail := uniqueSnapshotTail(timestamped)
		if tail == "" {
			return coordinates{}, false
		}
		version = prefix + tail
	default:
		return coordinates{}, false
	}

	remainder := rest[len(version):]
	result := coordinates{
		ArtifactID:  artifactID,
		Version:     version,
		BaseVersion: baseVersion,
		Snapshot:    snapshot,
	}
	switch {
	case strings.HasPrefix(remainder, "."):
		result.Extension = remainder[1:]
	case strings.HasPrefix(remainder, "-"):
		classifier, extension, found := strings.Cut(remainder[1:], ".")
		if !found || classifier == "" {
			return coordinates{}, false
		}
		result.Classifier = classifier
		result.Extension = extension
	default:
		return coordinates{}, false
	}
	if result.Extension == "" {
		return coordinates{}, false
	}
	return result, true
}

// uniqueSnapshotTail extracts the leading {yyyymmdd.hhmmss}-{build} portion of
// value, returning "" when the value does not start with a valid unique
// snapshot suffix.
func uniqueSnapshotTail(value string) string {
	// The timestamp-build tail runs until the classifier ("-") or extension
	// (".") boundary; scan candidate cut points from the shortest match.
	for index := 0; index <= len(value); index++ {
		if index == len(value) || value[index] == '.' || value[index] == '-' {
			candidate := value[:index]
			if uniqueSnapshotPattern.MatchString(candidate) {
				return candidate
			}
		}
	}
	return ""
}
