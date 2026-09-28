package maven

import (
	"fmt"
	"testing"
)

func snapshotMergeSource(timestamp string, build int, updated, classifier, value string) []byte {
	if value == "" {
		value = fmt.Sprintf("1.0-%s-%d", timestamp, build)
	}
	classifierXML := ""
	if classifier != "" {
		classifierXML = "<classifier>" + classifier + "</classifier>"
	}
	return []byte(fmt.Sprintf(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version><versioning><snapshot><timestamp>%s</timestamp><buildNumber>%d</buildNumber></snapshot><snapshotVersions><snapshotVersion>%s<extension>jar</extension><value>%s</value><updated>%s</updated></snapshotVersion></snapshotVersions></versioning></metadata>`, timestamp, build, classifierXML, value, updated))
}

func TestMergeSnapshotBuildsEqualUpdated(t *testing.T) {
	older := snapshotMergeSource("20260927.120000", 9, "20260927120000", "", "")
	newerBuild := snapshotMergeSource("20260927.120000", 10, "20260927120000", "", "")
	newerTimestamp := snapshotMergeSource("20260927.120001", 1, "20260927120000", "", "")
	sourcesClassifier := snapshotMergeSource("20260927.120000", 9, "20260927120000", "sources", "")
	laterUpdate := snapshotMergeSource("20260927.120000", 9, "20260927120001", "", "")
	malformed := snapshotMergeSource("20260927.120000", 9, "20260927120000", "", "1.0-20260927.120000-unknown")
	localCopy := snapshotMergeSource("20260927.120000", 9, "20260927120000", "", "1.0-SNAPSHOT")
	cases := []struct {
		name          string
		sources       [][]byte
		wantTimestamp string
		wantBuild     int
		wantJar       string
		wantSources   string
	}{
		{"build-9-then-10", [][]byte{older, newerBuild, sourcesClassifier}, "20260927.120000", 10, "1.0-20260927.120000-10", "1.0-20260927.120000-9"},
		{"build-10-then-9", [][]byte{newerBuild, older, sourcesClassifier}, "20260927.120000", 10, "1.0-20260927.120000-10", "1.0-20260927.120000-9"},
		{"timestamp-then-build", [][]byte{newerTimestamp, newerBuild}, "20260927.120001", 1, "1.0-20260927.120001-1", ""},
		{"build-then-timestamp", [][]byte{newerBuild, newerTimestamp}, "20260927.120001", 1, "1.0-20260927.120001-1", ""},
		{"updated-remains-primary", [][]byte{newerBuild, laterUpdate}, "20260927.120000", 10, "1.0-20260927.120000-9", ""},
		{"malformed-first-keeps-member-order", [][]byte{malformed, newerBuild}, "20260927.120000", 10, "1.0-20260927.120000-unknown", ""},
		{"local-copy-first-keeps-member-order", [][]byte{localCopy, newerBuild}, "20260927.120000", 10, "1.0-SNAPSHOT", ""},
		{"malformed-later-keeps-member-order", [][]byte{newerBuild, malformed}, "20260927.120000", 10, "1.0-20260927.120000-10", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := mergeMetadata(tc.sources)
			if err != nil {
				t.Fatal(err)
			}
			if merged.Versioning.Snapshot.Timestamp != tc.wantTimestamp || merged.Versioning.Snapshot.BuildNumber != tc.wantBuild {
				t.Errorf("snapshot = %+v, want %s build %d", merged.Versioning.Snapshot, tc.wantTimestamp, tc.wantBuild)
			}
			entries := map[string]string{}
			for _, entry := range merged.Versioning.SnapshotVersions.SnapshotVersions {
				entries[entry.Classifier+"/"+entry.Extension] = entry.Value
			}
			if got := entries["/jar"]; got != tc.wantJar {
				t.Errorf("jar = %q, want %q", got, tc.wantJar)
			}
			if got := entries["sources/jar"]; got != tc.wantSources {
				t.Errorf("sources = %q, want %q", got, tc.wantSources)
			}
			wantEntries := 1
			if tc.wantSources != "" {
				wantEntries++
			}
			if len(entries) != wantEntries {
				t.Errorf("unexpected entries: %+v", entries)
			}
		})
	}
}

func TestMetadataSnapshotSuffixRejectsUnrecognizedValues(t *testing.T) {
	for _, value := range []string{"1.0-SNAPSHOT", "1.0-20260927.120000-unknown", "2.0-20260927.120000-10"} {
		suffix, ok := metadataSnapshotSuffix("1.0-SNAPSHOT", value)
		if ok || suffix != "" {
			t.Errorf("%q parsed as %q, %t", value, suffix, ok)
		}
	}
}
