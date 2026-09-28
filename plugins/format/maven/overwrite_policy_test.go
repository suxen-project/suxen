package maven

import (
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestMutableHostedPathKeepsMavenIndexesPublishable(t *testing.T) {
	plugin := Format{}
	repository := format.Repository{Type: "hosted", AllowOverwrite: false}
	for _, path := range []string{
		"org/example/widget/maven-metadata.xml",
		"org/example/widget/maven-metadata.xml.sha256",
		"org/example/widget/maven-metadata.xml.asc",
		"org/example/widget/1.0-SNAPSHOT/maven-metadata.xml.sha1",
	} {
		if !plugin.MutableHostedPath(repository, path) {
			t.Fatalf("metadata path %q must be mutable", path)
		}
	}
	for _, path := range []string{
		"org/example/widget/1.0/widget-1.0.jar",
		"org/example/widget/1.0/widget-1.0.jar.sha1",
		"org/example/widget/1.0/widget-1.0.jar.asc",
	} {
		if plugin.MutableHostedPath(repository, path) {
			t.Fatalf("artifact path %q must remain protected", path)
		}
	}
}
