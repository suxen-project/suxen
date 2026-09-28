package maven

import (
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestRetentionDirectoryAndGroup(t *testing.T) {
	f := Format{}
	repository := format.Repository{Type: "hosted"}
	for _, test := range []struct{ path, directory, group string }{
		{"org/example/widget/1.0/widget-1.0.pom", "org/example/widget/1.0", "org.example/widget"},
		{"org/example/widget/1.0/widget-1.0-sources.jar.asc", "org/example/widget/1.0", "org.example/widget"},
		{"org/widget/1.0-SNAPSHOT/maven-metadata.xml.sha1", "org/widget/1.0-SNAPSHOT", ""},
		{"org/example/widget-SNAPSHOT/maven-metadata.xml", "org/example/widget-SNAPSHOT", ""},
		{"org/widget/maven-metadata.xml", "", ""},
		{"org/example/widget/maven-metadata.xml", "", ""},
		{"org/widget/1.0/maven-metadata.xml", "", ""},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := f.RetentionUnitDirectory(repository, test.path); got != test.directory {
				t.Errorf("directory = %q, want %q", got, test.directory)
			}
			group, ok := f.RetentionGroupKey(repository, format.Asset{Path: test.path})
			if group != test.group || ok != (test.group != "") {
				t.Errorf("group = %q, %v; want %q", group, ok, test.group)
			}
		})
	}
}

func TestRetentionAnchorRequiresArtifact(t *testing.T) {
	f := Format{}
	repository := format.Repository{Type: "hosted"}
	for _, test := range []struct {
		path string
		want bool
	}{
		{"org/widget/1.0-SNAPSHOT/widget-1.0-20260807.120000-1.jar", true},
		{"org/widget/1.0-SNAPSHOT/widget-1.0-20260807.120000-1.jar.sha1", false},
		{"org/widget/1.0-SNAPSHOT/widget-1.0-20260807.120000-1.jar.asc", false},
		{"org/widget/1.0-SNAPSHOT/maven-metadata.xml", false},
		{"org/example/widget-SNAPSHOT/maven-metadata.xml", false},
	} {
		if got := f.IsRetentionUnitAnchor(repository, test.path); got != test.want {
			t.Errorf("anchor(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}
