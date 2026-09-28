package maven

import (
	"encoding/xml"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strconv"
	"strings"
)

// metadataDocument models the org.apache.maven.artifact.repository.metadata
// schema: artifact-level (versions), version-level (snapshot builds), and
// group-level (plugin prefixes) indexes share the one <metadata> root.
type metadataDocument struct {
	XMLName    xml.Name    `xml:"metadata"`
	GroupID    string      `xml:"groupId,omitempty"`
	ArtifactID string      `xml:"artifactId,omitempty"`
	Version    string      `xml:"version,omitempty"`
	Versioning *versioning `xml:"versioning,omitempty"`
	Plugins    *pluginList `xml:"plugins,omitempty"`
}

type versioning struct {
	Latest           string               `xml:"latest,omitempty"`
	Release          string               `xml:"release,omitempty"`
	Snapshot         *snapshotBlock       `xml:"snapshot,omitempty"`
	Versions         *versionList         `xml:"versions,omitempty"`
	LastUpdated      string               `xml:"lastUpdated,omitempty"`
	SnapshotVersions *snapshotVersionList `xml:"snapshotVersions,omitempty"`
}

type versionList struct {
	Versions []string `xml:"version"`
}

type snapshotBlock struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
	LocalCopy   bool   `xml:"localCopy,omitempty"`
}

type snapshotVersionList struct {
	SnapshotVersions []snapshotVersion `xml:"snapshotVersion"`
}

type snapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension,omitempty"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated,omitempty"`
}

type pluginList struct {
	Plugins []mavenPlugin `xml:"plugin"`
}

type mavenPlugin struct {
	Name       string `xml:"name,omitempty"`
	Prefix     string `xml:"prefix"`
	ArtifactID string `xml:"artifactId"`
}

// mergeMetadata merges member metadata documents in member order. Malformed
// sources are skipped; merging fails only when no source parses.
func mergeMetadata(sources [][]byte) (*metadataDocument, error) {
	documents := make([]*metadataDocument, 0, len(sources))
	for _, source := range sources {
		document := &metadataDocument{}
		if err := xml.Unmarshal(source, document); err != nil {
			continue
		}
		documents = append(documents, document)
	}
	if len(documents) == 0 {
		return nil, errors.New("no group member served parseable maven metadata")
	}

	merged := &metadataDocument{
		GroupID:    documents[0].GroupID,
		ArtifactID: documents[0].ArtifactID,
		Version:    documents[0].Version,
	}
	mergeVersionLists(merged, documents)
	mergeSnapshotBuilds(merged, documents)
	mergePluginPrefixes(merged, documents)
	return merged, nil
}

// mergeVersionLists unions artifact-level version lists and recomputes the
// latest/release pointers from the merged set.
func mergeVersionLists(merged *metadataDocument, documents []*metadataDocument) {
	seen := make(map[string]struct{})
	versions := make([]string, 0)
	lastUpdated := ""
	for _, document := range documents {
		if document.Versioning == nil {
			continue
		}
		if document.Versioning.LastUpdated > lastUpdated {
			lastUpdated = document.Versioning.LastUpdated
		}
		if document.Versioning.Versions == nil {
			continue
		}
		for _, version := range document.Versioning.Versions.Versions {
			if _, duplicate := seen[version]; duplicate || version == "" {
				continue
			}
			seen[version] = struct{}{}
			versions = append(versions, version)
		}
	}
	if len(versions) == 0 {
		if lastUpdated != "" {
			ensureVersioning(merged).LastUpdated = lastUpdated
		}
		return
	}
	sort.Slice(versions, func(left, right int) bool {
		return compareVersions(versions[left], versions[right]) < 0
	})

	versioning := ensureVersioning(merged)
	versioning.Versions = &versionList{Versions: versions}
	versioning.LastUpdated = lastUpdated
	versioning.Latest = versions[len(versions)-1]
	for index := len(versions) - 1; index >= 0; index-- {
		if !strings.HasSuffix(versions[index], "-SNAPSHOT") {
			versioning.Release = versions[index]
			break
		}
	}
}

// mergeSnapshotBuilds selects the newest snapshot build across members and
// unions the per-classifier snapshot version entries, keeping the newest of
// each (classifier, extension) pair.
func mergeSnapshotBuilds(merged *metadataDocument, documents []*metadataDocument) {
	var newest *snapshotBlock
	for _, document := range documents {
		if document.Versioning == nil || document.Versioning.Snapshot == nil {
			continue
		}
		candidate := document.Versioning.Snapshot
		if newest == nil || snapshotNewer(candidate, newest) {
			newest = candidate
		}
	}

	type snapshotKey struct{ classifier, extension string }
	entries := make(map[snapshotKey]snapshotVersion)
	suffixes := make(map[snapshotKey]string)
	order := make([]snapshotKey, 0)
	for _, document := range documents {
		if document.Versioning == nil || document.Versioning.SnapshotVersions == nil {
			continue
		}
		for _, entry := range document.Versioning.SnapshotVersions.SnapshotVersions {
			key := snapshotKey{classifier: entry.Classifier, extension: entry.Extension}
			existing, present := entries[key]
			if !present {
				order = append(order, key)
				entries[key] = entry
				suffixes[key], _ = metadataSnapshotSuffix(document.Version, entry.Value)
				continue
			}
			candidateSuffix, candidateTimestamped := metadataSnapshotSuffix(document.Version, entry.Value)
			currentSuffix := suffixes[key]
			if entry.Updated > existing.Updated ||
				(entry.Updated == existing.Updated && candidateTimestamped && currentSuffix != "" &&
					uniqueSuffixNewer(candidateSuffix, currentSuffix)) {
				entries[key] = entry
				suffixes[key] = candidateSuffix
			}
		}
	}

	if newest == nil && len(entries) == 0 {
		return
	}
	versioning := ensureVersioning(merged)
	versioning.Snapshot = newest
	if len(entries) > 0 {
		list := &snapshotVersionList{}
		for _, key := range order {
			list.SnapshotVersions = append(list.SnapshotVersions, entries[key])
		}
		versioning.SnapshotVersions = list
	}
}

// metadataSnapshotSuffix extracts a timestamped version only when it belongs
// to the document's SNAPSHOT base version. Entries with local-copy or malformed
// values cannot safely break an Updated tie and keep member-order precedence.
func metadataSnapshotSuffix(baseVersion, value string) (string, bool) {
	if !strings.HasSuffix(baseVersion, "-SNAPSHOT") {
		return "", false
	}
	prefix := strings.TrimSuffix(baseVersion, "SNAPSHOT")
	suffix, ok := strings.CutPrefix(value, prefix)
	if !ok || !uniqueSnapshotPattern.MatchString(suffix) {
		return "", false
	}
	return suffix, true
}

func mergePluginPrefixes(merged *metadataDocument, documents []*metadataDocument) {
	seen := make(map[string]struct{})
	plugins := make([]mavenPlugin, 0)
	for _, document := range documents {
		if document.Plugins == nil {
			continue
		}
		for _, plugin := range document.Plugins.Plugins {
			if _, duplicate := seen[plugin.Prefix]; duplicate {
				continue
			}
			seen[plugin.Prefix] = struct{}{}
			plugins = append(plugins, plugin)
		}
	}
	if len(plugins) > 0 {
		merged.Plugins = &pluginList{Plugins: plugins}
	}
}

func snapshotNewer(candidate, current *snapshotBlock) bool {
	if candidate.Timestamp != current.Timestamp {
		return candidate.Timestamp > current.Timestamp
	}
	return candidate.BuildNumber > current.BuildNumber
}

func ensureVersioning(document *metadataDocument) *versioning {
	if document.Versioning == nil {
		document.Versioning = &versioning{}
	}
	return document.Versioning
}

func renderMetadata(document *metadataDocument) ([]byte, error) {
	body, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render maven metadata: %w", err)
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

// synthesizeArtifactMetadata derives artifact-level metadata from the version
// directories that hold valid artifact files directly under directory.
func synthesizeArtifactMetadata(
	directory string,
	paths iter.Seq[string],
) (*metadataDocument, bool) {
	seen := make(map[string]struct{})
	versions := make([]string, 0)
	newestSnapshot := ""
	groupID := ""
	artifactID := ""
	for assetPath := range paths {
		coordinatesOf, ok := directArtifact(directory, assetPath, 2)
		if !ok {
			continue
		}
		groupID = coordinatesOf.GroupID
		artifactID = coordinatesOf.ArtifactID
		if coordinatesOf.Snapshot && coordinatesOf.Version != coordinatesOf.BaseVersion {
			timestamped := uniqueSnapshotSuffix(coordinatesOf)
			if uniqueSuffixNewer(timestamped, newestSnapshot) {
				newestSnapshot = timestamped
			}
		}
		if _, duplicate := seen[coordinatesOf.BaseVersion]; duplicate {
			continue
		}
		seen[coordinatesOf.BaseVersion] = struct{}{}
		versions = append(versions, coordinatesOf.BaseVersion)
	}
	if len(versions) == 0 {
		return nil, false
	}
	sort.Slice(versions, func(left, right int) bool {
		return compareVersions(versions[left], versions[right]) < 0
	})

	document := &metadataDocument{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Versioning: &versioning{
			Versions: &versionList{Versions: versions},
			Latest:   versions[len(versions)-1],
		},
	}
	if newestSnapshot != "" {
		document.Versioning.LastUpdated = strings.ReplaceAll(timestampOf(newestSnapshot), ".", "")
	}
	for index := len(versions) - 1; index >= 0; index-- {
		if !strings.HasSuffix(versions[index], "-SNAPSHOT") {
			document.Versioning.Release = versions[index]
			break
		}
	}
	return document, true
}

// synthesizeVersionMetadata derives version-level snapshot metadata from the
// timestamped snapshot files directly under a *-SNAPSHOT directory.
func synthesizeVersionMetadata(
	directory string,
	paths iter.Seq[string],
) (*metadataDocument, bool) {
	type snapshotKey struct{ classifier, extension string }
	entries := make(map[snapshotKey]snapshotVersion)
	suffixes := make(map[snapshotKey]string)
	order := make([]snapshotKey, 0)
	newest := ""
	groupID := ""
	artifactID := ""
	baseVersion := ""
	for assetPath := range paths {
		coordinatesOf, ok := directArtifact(directory, assetPath, 1)
		if !ok || !coordinatesOf.Snapshot ||
			coordinatesOf.Version == coordinatesOf.BaseVersion {
			continue
		}
		groupID = coordinatesOf.GroupID
		artifactID = coordinatesOf.ArtifactID
		baseVersion = coordinatesOf.BaseVersion

		timestamped := uniqueSnapshotSuffix(coordinatesOf)
		if uniqueSuffixNewer(timestamped, newest) {
			newest = timestamped
		}
		key := snapshotKey{
			classifier: coordinatesOf.Classifier,
			extension:  coordinatesOf.Extension,
		}
		entry := snapshotVersion{
			Classifier: coordinatesOf.Classifier,
			Extension:  coordinatesOf.Extension,
			Value:      coordinatesOf.Version,
			Updated:    strings.ReplaceAll(timestampOf(timestamped), ".", ""),
		}
		if _, present := entries[key]; !present {
			order = append(order, key)
			entries[key] = entry
			suffixes[key] = timestamped
			continue
		}
		if uniqueSuffixNewer(timestamped, suffixes[key]) {
			entries[key] = entry
			suffixes[key] = timestamped
		}
	}
	if len(entries) == 0 {
		return nil, false
	}

	build := 0
	if _, buildText, found := strings.Cut(newest, "-"); found {
		build, _ = strconv.Atoi(buildText)
	}
	list := &snapshotVersionList{}
	for _, key := range order {
		list.SnapshotVersions = append(list.SnapshotVersions, entries[key])
	}
	return &metadataDocument{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Version:    baseVersion,
		Versioning: &versioning{
			Snapshot: &snapshotBlock{
				Timestamp:   timestampOf(newest),
				BuildNumber: build,
			},
			LastUpdated:      strings.ReplaceAll(timestampOf(newest), ".", ""),
			SnapshotVersions: list,
		},
	}, true
}

// directArtifact parses an asset path and requires it to sit exactly depth
// path segments below directory, so listings do not leak nested artifacts
// into a parent index.
func directArtifact(directory string, assetPath string, depth int) (coordinates, bool) {
	relative, found := strings.CutPrefix(assetPath, directory)
	if !found || relative == "" ||
		len(strings.Split(relative, "/")) != depth {
		return coordinates{}, false
	}
	info, ok := parsePath(assetPath)
	if !ok || info.Coordinates == nil || info.Checksum || info.Signature {
		return coordinates{}, false
	}
	return *info.Coordinates, true
}

// uniqueSnapshotSuffix returns the {yyyymmdd.hhmmss}-{build} tail of a unique
// snapshot file version.
func uniqueSnapshotSuffix(c coordinates) string {
	prefix := strings.TrimSuffix(c.BaseVersion, "SNAPSHOT")
	return strings.TrimPrefix(c.Version, prefix)
}

func timestampOf(uniqueSuffix string) string {
	timestamp, _, _ := strings.Cut(uniqueSuffix, "-")
	return timestamp
}

// uniqueSuffixNewer orders {yyyymmdd.hhmmss}-{build} suffixes by timestamp,
// then by numeric build number (lexical order fails past build 9).
func uniqueSuffixNewer(candidate, current string) bool {
	if current == "" {
		return candidate != ""
	}
	candidateTimestamp, candidateBuildText, _ := strings.Cut(candidate, "-")
	currentTimestamp, currentBuildText, _ := strings.Cut(current, "-")
	if candidateTimestamp != currentTimestamp {
		return candidateTimestamp > currentTimestamp
	}
	candidateBuild, _ := strconv.Atoi(candidateBuildText)
	currentBuild, _ := strconv.Atoi(currentBuildText)
	return candidateBuild > currentBuild
}
