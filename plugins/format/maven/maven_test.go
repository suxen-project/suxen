package maven

import (
	"errors"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestParsePathArtifacts(t *testing.T) {
	cases := map[string]coordinates{
		"com/example/app/1.0/app-1.0.jar": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0", BaseVersion: "1.0", Extension: "jar",
		},
		"com/example/app/1.0/app-1.0.pom": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0", BaseVersion: "1.0", Extension: "pom",
		},
		"org/x/lib-core/2.3.1/lib-core-2.3.1-sources.jar": {
			GroupID: "org.x", ArtifactID: "lib-core",
			Version: "2.3.1", BaseVersion: "2.3.1",
			Classifier: "sources", Extension: "jar",
		},
		"com/example/app/1.0/app-1.0-dist.tar.gz": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0", BaseVersion: "1.0",
			Classifier: "dist", Extension: "tar.gz",
		},
		"com/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0-SNAPSHOT", BaseVersion: "1.0-SNAPSHOT",
			Extension: "jar", Snapshot: true,
		},
		"com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-3.jar": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0-20260807.120000-3", BaseVersion: "1.0-SNAPSHOT",
			Extension: "jar", Snapshot: true,
		},
		"com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-12-sources.jar": {
			GroupID: "com.example", ArtifactID: "app",
			Version: "1.0-20260807.120000-12", BaseVersion: "1.0-SNAPSHOT",
			Classifier: "sources", Extension: "jar", Snapshot: true,
		},
	}
	for assetPath, want := range cases {
		info, ok := parsePath(assetPath)
		if !ok {
			t.Fatalf("parsePath(%q) rejected a valid path", assetPath)
		}
		if info.Metadata || info.Checksum || info.Signature {
			t.Fatalf("parsePath(%q) misclassified: %+v", assetPath, info)
		}
		if info.Coordinates == nil || *info.Coordinates != want {
			t.Fatalf("parsePath(%q) = %+v, want %+v", assetPath, info.Coordinates, want)
		}
	}
}

func TestParsePathCompanionsAndMetadata(t *testing.T) {
	checksum, ok := parsePath("com/example/app/1.0/app-1.0.jar.sha1")
	if !ok || !checksum.Checksum || checksum.Coordinates == nil {
		t.Fatalf("checksum parse = %+v ok=%v", checksum, ok)
	}
	if checksum.Coordinates.Extension != "jar" {
		t.Fatalf("checksum base extension = %q", checksum.Coordinates.Extension)
	}

	signature, ok := parsePath("com/example/app/1.0/app-1.0.jar.asc")
	if !ok || !signature.Signature || signature.Coordinates == nil {
		t.Fatalf("signature parse = %+v ok=%v", signature, ok)
	}
	signedChecksum, ok := parsePath("com/example/app/1.0/app-1.0.jar.asc.sha1")
	if !ok || !signedChecksum.Signature || !signedChecksum.Checksum {
		t.Fatalf("signature checksum parse = %+v ok=%v", signedChecksum, ok)
	}

	artifactMetadata, ok := parsePath("com/example/app/maven-metadata.xml")
	if !ok || !artifactMetadata.Metadata || artifactMetadata.SnapshotVersionDirectory {
		t.Fatalf("artifact metadata parse = %+v ok=%v", artifactMetadata, ok)
	}
	versionMetadata, ok := parsePath("com/example/app/1.0-SNAPSHOT/maven-metadata.xml")
	if !ok || !versionMetadata.Metadata || !versionMetadata.SnapshotVersionDirectory {
		t.Fatalf("version metadata parse = %+v ok=%v", versionMetadata, ok)
	}
	metadataChecksum, ok := parsePath("com/example/app/maven-metadata.xml.sha512")
	if !ok || !metadataChecksum.Metadata || !metadataChecksum.Checksum {
		t.Fatalf("metadata checksum parse = %+v ok=%v", metadataChecksum, ok)
	}
	catalog, ok := parsePath("archetype-catalog.xml")
	if !ok || !catalog.Metadata {
		t.Fatalf("archetype catalog parse = %+v ok=%v", catalog, ok)
	}
}

func TestParsePathRejectsForeignLayouts(t *testing.T) {
	rejected := []string{
		"",
		"file.jar",
		"com/example/file.jar",
		"com/example/app/1.0/other-1.0.jar",
		"com/example/app/1.0/app-2.0.jar",
		"com/example/app/1.0/app-1.0",
		"com/example/app/1.0-SNAPSHOT/app-1.0-2026.jar",
		"com/example/app/1.0/app-1.0-.jar",
	}
	for _, assetPath := range rejected {
		if info, ok := parsePath(assetPath); ok {
			t.Fatalf("parsePath(%q) accepted a foreign path: %+v", assetPath, info)
		}
	}
}

func repositoryWithPolicy(policy string) format.Repository {
	repository := format.Repository{Name: "maven-test", Format: "maven", Type: "hosted"}
	if policy != "" {
		repository.Config = map[string]any{"versionPolicy": policy}
	}
	return repository
}

func TestValidateRepository(t *testing.T) {
	f := Format{}
	for _, policy := range []string{"", policyRelease, policySnapshot, policyMixed} {
		if err := f.ValidateRepository(repositoryWithPolicy(policy)); err != nil {
			t.Fatalf("ValidateRepository(policy=%q): %v", policy, err)
		}
	}

	var violation *format.PolicyViolation
	err := f.ValidateRepository(repositoryWithPolicy("weekly"))
	if !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("invalid policy error = %v", err)
	}
	err = f.ValidateRepository(format.Repository{
		Config: map[string]any{"unknownKey": true},
	})
	if !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("unknown config key error = %v", err)
	}
}

func TestValidateUploadVersionPolicy(t *testing.T) {
	f := Format{}
	release := "com/example/app/1.0/app-1.0.jar"
	snapshot := "com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-1.jar"
	metadata := "com/example/app/maven-metadata.xml"
	snapshotMetadata := "com/example/app/1.0-SNAPSHOT/maven-metadata.xml"

	releases := repositoryWithPolicy(policyRelease)
	if err := f.ValidateUpload(releases, release); err != nil {
		t.Fatalf("release upload into release repository: %v", err)
	}
	if err := f.ValidateUpload(releases, metadata); err != nil {
		t.Fatalf("metadata upload into release repository: %v", err)
	}
	var violation *format.PolicyViolation
	if err := f.ValidateUpload(releases, snapshot); !errors.As(err, &violation) ||
		violation.Code != "maven_version_policy" {
		t.Fatalf("snapshot into release repository error = %v", err)
	}
	if err := f.ValidateUpload(releases, snapshotMetadata); err != nil {
		t.Fatalf("ambiguous metadata path into release repository: %v", err)
	}
	artifactIDSnapshotMetadata := "com/example/widget-SNAPSHOT/maven-metadata.xml"
	if err := f.ValidateUpload(releases, artifactIDSnapshotMetadata); err != nil {
		t.Fatalf("release artifact index with SNAPSHOT-suffixed artifact ID: %v", err)
	}
	if err := f.ValidateProxyPath(releases, artifactIDSnapshotMetadata); err != nil {
		t.Fatalf("release proxy artifact index with SNAPSHOT-suffixed artifact ID: %v", err)
	}

	snapshots := repositoryWithPolicy(policySnapshot)
	if err := f.ValidateUpload(snapshots, snapshot); err != nil {
		t.Fatalf("snapshot upload into snapshot repository: %v", err)
	}
	if err := f.ValidateUpload(snapshots, metadata); err != nil {
		t.Fatalf("metadata upload into snapshot repository: %v", err)
	}
	if err := f.ValidateUpload(snapshots, release); !errors.As(err, &violation) ||
		violation.Code != "maven_version_policy" {
		t.Fatalf("release into snapshot repository error = %v", err)
	}

	mixed := repositoryWithPolicy("")
	for _, assetPath := range []string{release, snapshot, metadata, snapshotMetadata} {
		if err := f.ValidateUpload(mixed, assetPath); err != nil {
			t.Fatalf("mixed repository rejected %q: %v", assetPath, err)
		}
	}

	if err := f.ValidateUpload(mixed, "not/a/maven-path.random"); !errors.As(err, &violation) ||
		violation.Code != "maven_invalid_path" {
		t.Fatalf("foreign path error = %v", err)
	}
}

func TestProjectAttributes(t *testing.T) {
	f := Format{}
	attributes := f.ProjectAttributes(format.Asset{
		Path: "com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-3-sources.jar",
	})
	want := map[string]any{
		"groupId":     "com.example",
		"artifactId":  "app",
		"version":     "1.0-20260807.120000-3",
		"baseVersion": "1.0-SNAPSHOT",
		"classifier":  "sources",
		"extension":   "jar",
		"snapshot":    true,
	}
	for key, value := range want {
		if attributes[key] != value {
			t.Fatalf("attribute %s = %v, want %v (all: %v)", key, attributes[key], value, attributes)
		}
	}

	metadata := f.ProjectAttributes(format.Asset{Path: "com/example/app/maven-metadata.xml"})
	if metadata["metadata"] != true {
		t.Fatalf("metadata attributes = %v", metadata)
	}
	if f.ProjectAttributes(format.Asset{Path: "unparseable"}) != nil {
		t.Fatal("foreign paths must project no attributes")
	}
}

func TestMutableUpstreamPath(t *testing.T) {
	f := Format{}
	repository := repositoryWithPolicy("")
	mutable := []string{
		"com/example/app/maven-metadata.xml",
		"com/example/app/maven-metadata.xml.sha1",
		"com/example/app/1.0-SNAPSHOT/maven-metadata.xml",
		"com/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar",
		"archetype-catalog.xml",
	}
	for _, assetPath := range mutable {
		if !f.MutableUpstreamPath(repository, assetPath) {
			t.Fatalf("%q must be mutable", assetPath)
		}
	}
	immutable := []string{
		"com/example/app/1.0/app-1.0.jar",
		"com/example/app/1.0/app-1.0.jar.sha1",
		"com/example/app/1.0-SNAPSHOT/app-1.0-20260807.120000-3.jar",
		"unparseable/path.bin",
	}
	for _, assetPath := range immutable {
		if f.MutableUpstreamPath(repository, assetPath) {
			t.Fatalf("%q must be immutable", assetPath)
		}
	}
}

func TestFormatRegistration(t *testing.T) {
	if !format.Registered("maven") {
		t.Fatal("maven format is not registered")
	}
	registered, found := format.Lookup("maven")
	if !found || registered.Name() != "maven" {
		t.Fatalf("Lookup(maven) = %v, %v", registered, found)
	}
}
