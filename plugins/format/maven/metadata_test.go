package maven

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	ordered := [][2]string{
		{"1.0-alpha", "1.0-beta"},
		{"1.0-beta", "1.0-rc1"},
		{"1.0-rc1", "1.0-SNAPSHOT"},
		{"1.0-SNAPSHOT", "1.0"},
		{"1.0", "1.0-sp"},
		{"1.0", "1.0.1"},
		{"1.0-2", "1.0.1"},
		{"1.0.0.RC2", "1.0.0-RC3"},
		{"1.0-alpha", "1.0-a1"},
		{"1.0-b1", "1.0-milestone"},
		{"1.0-cr1", "1.0-SNAPSHOT"},
		{"1.0-sp", "1.0-2"},
		{"1-sp-1", "1-ga-1"},
		{"1.0", "1_0"}, // Maven 3.9 treats '_' as a qualifier character.
		{"1.99999999999999999999999999999999999999", "1.100000000000000000000000000000000000000"},
		{"1.0.1", "1.1"},
		{"1.9", "1.10"},
		{"1.10", "2.0"},
		{"2.0", "10.0"},
	}
	for _, pair := range ordered {
		if compareVersions(pair[0], pair[1]) >= 0 {
			t.Fatalf("compareVersions(%q, %q) >= 0, want < 0", pair[0], pair[1])
		}
		if compareVersions(pair[1], pair[0]) <= 0 {
			t.Fatalf("compareVersions(%q, %q) <= 0, want > 0", pair[1], pair[0])
		}
	}
	equal := [][2]string{
		{"1.0", "1.0.0"}, {"1.0", "1.0"}, {"1.0-GA", "1.0.final"},
		{"1.0-a1", "1.0-alpha-1"}, {"1.0-CR", "1.0-rc"},
		{"1.0.0-0.0.0", "1"},
	}
	for _, pair := range equal {
		if compareVersions(pair[0], pair[1]) != 0 {
			t.Fatalf("compareVersions(%q, %q) != 0", pair[0], pair[1])
		}
	}
}

func TestMavenNumericQualifierMetadata(t *testing.T) {
	paths := []string{
		"com/example/app/1.0-2/app-1.0-2.jar",
		"com/example/app/1.0.1/app-1.0.1.jar",
	}
	document, ok := synthesizeArtifactMetadata("com/example/app/", slices.Values(paths))
	if !ok {
		t.Fatal("hosted metadata absent")
	}
	if document.Versioning.Latest != "1.0.1" || document.Versioning.Release != "1.0.1" {
		t.Fatalf("hosted latest/release = %q/%q", document.Versioning.Latest, document.Versioning.Release)
	}
	first := []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><versioning><versions><version>1.0-2</version></versions></versioning></metadata>`)
	second := []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><versioning><versions><version>1.0.1</version></versions></versioning></metadata>`)
	merged, err := mergeMetadata([][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Versioning.Latest != "1.0.1" || merged.Versioning.Release != "1.0.1" {
		t.Fatalf("group latest/release = %q/%q", merged.Versioning.Latest, merged.Versioning.Release)
	}
}

func TestMavenUnicodeDigitMetadataOrdering(t *testing.T) {
	// Maven treats Arabic-Indic ٢ as numeric 2. The former byte-oriented
	// parser treated it as a qualifier and incorrectly selected 1.1.
	paths := []string{
		"com/example/app/1.1/app-1.1.jar",
		"com/example/app/1.٢/app-1.٢.jar",
	}
	hosted, ok := synthesizeArtifactMetadata("com/example/app/", slices.Values(paths))
	if !ok {
		t.Fatal("hosted metadata absent")
	}
	assertUnicodeLatest := func(label string, document *metadataDocument) {
		t.Helper()
		if document.Versioning.Latest != "1.٢" || document.Versioning.Release != "1.٢" {
			t.Fatalf("%s latest/release = %q/%q, want 1.٢", label, document.Versioning.Latest, document.Versioning.Release)
		}
		rendered, err := renderMetadata(document)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(rendered, []byte("<latest>1.٢</latest>")) ||
			!bytes.Contains(rendered, []byte("<release>1.٢</release>")) {
			t.Fatalf("%s rendered metadata lost Unicode latest/release: %s", label, rendered)
		}
	}
	assertUnicodeLatest("hosted", hosted)

	first := []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><versioning><versions><version>1.1</version></versions></versioning></metadata>`)
	second := []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><versioning><versions><version>1.٢</version></versions></versioning></metadata>`)
	merged, err := mergeMetadata([][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	assertUnicodeLatest("group", merged)
}

func TestCompareVersionsDeepNesting(t *testing.T) {
	// Project metadata is supplied by repositories and can contain a deeply
	// nested version. Comparison must not recurse on that input's depth.
	version := "1" + strings.Repeat("-x", 10000)
	if got := compareVersions(version, version); got != 0 {
		t.Fatalf("deep identical versions compare as %d", got)
	}
	if got := compareVersions(version, version+"-1"); got >= 0 {
		t.Fatalf("deep extension compares as %d, want negative", got)
	}
}

func TestMergeMetadataVersionLists(t *testing.T) {
	first := []byte(`<metadata>
  <groupId>com.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <latest>1.1</latest>
    <release>1.1</release>
    <versions><version>1.0</version><version>1.1</version></versions>
    <lastUpdated>20260801000000</lastUpdated>
  </versioning>
</metadata>`)
	second := []byte(`<metadata>
  <groupId>com.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <latest>2.0-SNAPSHOT</latest>
    <versions><version>1.1</version><version>2.0-SNAPSHOT</version></versions>
    <lastUpdated>20260807000000</lastUpdated>
  </versioning>
</metadata>`)

	merged, err := mergeMetadata([][]byte{first, second, []byte("not xml at all <")})
	if err != nil {
		t.Fatal(err)
	}
	if merged.GroupID != "com.example" || merged.ArtifactID != "app" {
		t.Fatalf("merged coordinates = %s:%s", merged.GroupID, merged.ArtifactID)
	}
	got := merged.Versioning.Versions.Versions
	want := []string{"1.0", "1.1", "2.0-SNAPSHOT"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("merged versions = %v, want %v", got, want)
	}
	if merged.Versioning.Latest != "2.0-SNAPSHOT" {
		t.Fatalf("latest = %q", merged.Versioning.Latest)
	}
	if merged.Versioning.Release != "1.1" {
		t.Fatalf("release = %q (snapshots are not releases)", merged.Versioning.Release)
	}
	if merged.Versioning.LastUpdated != "20260807000000" {
		t.Fatalf("lastUpdated = %q", merged.Versioning.LastUpdated)
	}

	if _, err := mergeMetadata([][]byte{[]byte("junk <")}); err == nil {
		t.Fatal("merging only malformed sources must fail")
	}
}

func TestMergeMetadataSnapshotBuilds(t *testing.T) {
	older := []byte(`<metadata>
  <groupId>com.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version>
  <versioning>
    <snapshot><timestamp>20260807.100000</timestamp><buildNumber>9</buildNumber></snapshot>
    <snapshotVersions>
      <snapshotVersion><extension>jar</extension><value>1.0-20260807.100000-9</value><updated>20260807100000</updated></snapshotVersion>
      <snapshotVersion><classifier>sources</classifier><extension>jar</extension><value>1.0-20260807.100000-9</value><updated>20260807100000</updated></snapshotVersion>
    </snapshotVersions>
  </versioning>
</metadata>`)
	newer := []byte(`<metadata>
  <groupId>com.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version>
  <versioning>
    <snapshot><timestamp>20260807.100000</timestamp><buildNumber>10</buildNumber></snapshot>
    <snapshotVersions>
      <snapshotVersion><extension>jar</extension><value>1.0-20260807.100000-10</value><updated>20260807100001</updated></snapshotVersion>
    </snapshotVersions>
  </versioning>
</metadata>`)

	merged, err := mergeMetadata([][]byte{older, newer})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := merged.Versioning.Snapshot
	if snapshot.BuildNumber != 10 {
		t.Fatalf("merged buildNumber = %d, want 10 (numeric, not lexical)", snapshot.BuildNumber)
	}
	entries := merged.Versioning.SnapshotVersions.SnapshotVersions
	if len(entries) != 2 {
		t.Fatalf("merged snapshotVersions = %v", entries)
	}
	byKey := map[string]string{}
	for _, entry := range entries {
		byKey[entry.Classifier+"/"+entry.Extension] = entry.Value
	}
	if byKey["/jar"] != "1.0-20260807.100000-10" {
		t.Fatalf("main jar entry = %q, want the newest build", byKey["/jar"])
	}
	if byKey["sources/jar"] != "1.0-20260807.100000-9" {
		t.Fatalf("sources entry = %q", byKey["sources/jar"])
	}
}

func TestMergeMetadataPluginPrefixes(t *testing.T) {
	first := []byte(`<metadata><plugins>
  <plugin><name>One</name><prefix>one</prefix><artifactId>one-maven-plugin</artifactId></plugin>
</plugins></metadata>`)
	second := []byte(`<metadata><plugins>
  <plugin><name>One Duplicate</name><prefix>one</prefix><artifactId>other</artifactId></plugin>
  <plugin><name>Two</name><prefix>two</prefix><artifactId>two-maven-plugin</artifactId></plugin>
</plugins></metadata>`)
	merged, err := mergeMetadata([][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	plugins := merged.Plugins.Plugins
	if len(plugins) != 2 || plugins[0].ArtifactID != "one-maven-plugin" ||
		plugins[1].Prefix != "two" {
		t.Fatalf("merged plugins = %v", plugins)
	}
}

func TestSynthesizeArtifactMetadata(t *testing.T) {
	paths := []string{
		"com/example/app/1.0/app-1.0.jar",
		"com/example/app/1.0/app-1.0.jar.sha1",
		"com/example/app/1.0/app-1.0.pom",
		"com/example/app/1.10/app-1.10.jar",
		"com/example/app/2.0-SNAPSHOT/app-2.0-20260807.110000-1.jar",
		"com/example/app/1.9/app-1.9.jar",
		// nested artifact and stray files must not leak into the index
		"com/example/app/nested/1.0/nested-1.0.jar",
		"com/example/app/maven-metadata.xml.asc",
	}
	document, ok := synthesizeArtifactMetadata("com/example/app/", slices.Values(paths))
	if !ok {
		t.Fatal("synthesis found no versions")
	}
	got := document.Versioning.Versions.Versions
	want := "1.0,1.9,1.10,2.0-SNAPSHOT"
	if strings.Join(got, ",") != want {
		t.Fatalf("versions = %v, want %s", got, want)
	}
	if document.Versioning.Latest != "2.0-SNAPSHOT" || document.Versioning.Release != "1.10" {
		t.Fatalf("latest/release = %q/%q", document.Versioning.Latest, document.Versioning.Release)
	}
	if document.GroupID != "com.example" || document.ArtifactID != "app" {
		t.Fatalf("coordinates = %s:%s", document.GroupID, document.ArtifactID)
	}
	if document.Versioning.LastUpdated != "20260807110000" {
		t.Fatalf("lastUpdated = %q, want newest stable snapshot timestamp", document.Versioning.LastUpdated)
	}
	first, err := renderMetadata(document)
	if err != nil {
		t.Fatal(err)
	}
	repeated, ok := synthesizeArtifactMetadata("com/example/app/", slices.Values(paths))
	if !ok {
		t.Fatal("repeated synthesis found no versions")
	}
	second, err := renderMetadata(repeated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("repeated synthesis changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	if _, ok := synthesizeArtifactMetadata("com/example/empty/", slices.Values([]string(nil))); ok {
		t.Fatal("synthesis without artifacts must report no content")
	}
	releaseOnly, ok := synthesizeArtifactMetadata("com/example/app/", slices.Values(paths[:4]))
	if !ok {
		t.Fatal("release-only synthesis found no versions")
	}
	if releaseOnly.Versioning.LastUpdated != "" {
		t.Fatalf("release-only lastUpdated = %q, want omitted without stable source", releaseOnly.Versioning.LastUpdated)
	}
}

func TestSynthesizeVersionMetadata(t *testing.T) {
	directory := "com/example/app/1.0-SNAPSHOT/"
	paths := []string{
		directory + "app-1.0-20260807.100000-9.jar",
		directory + "app-1.0-20260807.100000-9.pom",
		directory + "app-1.0-20260807.110000-10.jar",
		directory + "app-1.0-20260807.110000-10-sources.jar",
		directory + "app-1.0-20260807.110000-10.jar.sha1",
	}
	document, ok := synthesizeVersionMetadata(directory, slices.Values(paths))
	if !ok {
		t.Fatal("synthesis found no snapshot builds")
	}
	snapshot := document.Versioning.Snapshot
	if snapshot.Timestamp != "20260807.110000" || snapshot.BuildNumber != 10 {
		t.Fatalf("snapshot block = %+v", snapshot)
	}
	if document.Version != "1.0-SNAPSHOT" {
		t.Fatalf("version = %q", document.Version)
	}
	if document.Versioning.LastUpdated != "20260807110000" {
		t.Fatalf("lastUpdated = %q, want newest snapshot timestamp", document.Versioning.LastUpdated)
	}
	values := map[string]string{}
	for _, entry := range document.Versioning.SnapshotVersions.SnapshotVersions {
		values[entry.Classifier+"/"+entry.Extension] = entry.Value
	}
	if values["/jar"] != "1.0-20260807.110000-10" || values["/pom"] != "1.0-20260807.100000-9" {
		t.Fatalf("snapshotVersions = %v", values)
	}
	if values["sources/jar"] != "1.0-20260807.110000-10" {
		t.Fatalf("sources entry = %v", values)
	}
}

func TestMavenMetadataTarget(t *testing.T) {
	base, algorithm, ok := mavenMetadataTarget("com/example/app/maven-metadata.xml")
	if !ok || base != "com/example/app/maven-metadata.xml" || algorithm != "" {
		t.Fatalf("metadata target = %q %q %v", base, algorithm, ok)
	}
	base, algorithm, ok = mavenMetadataTarget("com/example/app/maven-metadata.xml.sha512")
	if !ok || base != "com/example/app/maven-metadata.xml" || algorithm != "sha512" {
		t.Fatalf("checksum target = %q %q %v", base, algorithm, ok)
	}
	for _, assetPath := range []string{
		"com/example/app/maven-metadata.xml.asc",
		"archetype-catalog.xml",
		"com/example/app/1.0/app-1.0.jar.sha1",
	} {
		if _, _, ok := mavenMetadataTarget(assetPath); ok {
			t.Fatalf("%q must not be a metadata target", assetPath)
		}
	}
}
