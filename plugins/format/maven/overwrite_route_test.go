package maven_test

import (
	"net/http"
	"testing"
)

func TestMavenNoOverwriteAllowsChangingIndexesButProtectsArtifacts(t *testing.T) {
	f := newFixture(t)
	mustStatus(t, f.createRepository(t, map[string]any{"name": "locked", "format": "maven", "type": "hosted", "allowOverwrite": false}), http.StatusCreated)
	put := func(path, body string, want int) {
		t.Helper()
		response := f.do(t, http.MethodPut, "/repository/locked/"+path, []byte(body), true)
		mustStatus(t, response, want)
	}
	artifact := "org/example/widget/1.0/widget-1.0.jar"
	put(artifact, "one", http.StatusCreated)
	put(artifact, "two", http.StatusConflict)
	put(artifact+".sha1", "first", http.StatusCreated)
	put(artifact+".sha1", "second", http.StatusConflict)
	metadata := "org/example/widget/maven-metadata.xml"
	put(metadata, "<metadata><versioning><latest>1.0</latest></versioning></metadata>", http.StatusCreated)
	put(metadata, "<metadata><versioning><latest>2.0</latest></versioning></metadata>", http.StatusCreated)
	put(metadata+".sha256", "first", http.StatusCreated)
	put(metadata+".sha256", "second", http.StatusCreated)
}
