package maven_test

import (
	"bytes"
	"net/http"
	"testing"
)

func TestGroupSnapshotMetadataSameSecondBuild(t *testing.T) {
	f := newFixture(t)
	for _, spec := range []map[string]any{
		{"name": "older", "format": "maven", "type": "hosted"},
		{"name": "newer", "format": "maven", "type": "hosted"},
		{"name": "group", "format": "maven", "type": "group", "members": []string{"older", "newer"}},
	} {
		mustStatus(t, f.createRepository(t, spec), http.StatusCreated)
	}
	metadata := func(build string) []byte {
		return []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version><versioning><snapshot><timestamp>20260927.120000</timestamp><buildNumber>` + build + `</buildNumber></snapshot><snapshotVersions><snapshotVersion><extension>jar</extension><value>1.0-20260927.120000-` + build + `</value><updated>20260927120000</updated></snapshotVersion></snapshotVersions></versioning></metadata>`)
	}
	for _, member := range []struct{ name, build string }{{"older", "9"}, {"newer", "10"}} {
		mustStatus(t, f.do(t, http.MethodPut, "/repository/"+member.name+"/com/example/app/1.0-SNAPSHOT/maven-metadata.xml", metadata(member.build), true), http.StatusCreated)
	}
	body := mustStatus(t, f.do(t, http.MethodGet, "/repository/group/com/example/app/1.0-SNAPSHOT/maven-metadata.xml", nil, true), http.StatusOK)
	if !bytes.Contains(body, []byte("<buildNumber>10</buildNumber>")) || !bytes.Contains(body, []byte("<value>1.0-20260927.120000-10</value>")) {
		t.Fatalf("group metadata header and jar disagree: %s", body)
	}
}
