package npm

import (
	"strings"
	"testing"
)

func TestHistoricalPackageNamesAreReadableButNotPublishable(t *testing.T) {
	for _, name := range []string{
		"JSONStream", "@Legacy/Widget", "old!name", "old~name", "old'name", "old(name)",
		".legacy", "_legacy", "-legacy", "@scope/.legacy", "node_modules", "favicon.ico",
		strings.Repeat("x", 215),
	} {
		if info, ok := parsePath(name); !ok || info.kind != kindPackument || info.name != name {
			t.Errorf("historical packument %q not recognized: %+v %v", name, info, ok)
		}
		file := name[strings.LastIndex(name, "/")+1:] + "-1.0.0.tgz"
		if info, ok := parsePath(tarballPath(name, file)); !ok || info.kind != kindTarball || info.name != name || info.version != "1.0.0" {
			t.Errorf("historical tarball %q not recognized: %+v %v", name, info, ok)
		}
		if validPublishPackageName(name) {
			t.Errorf("historical name %q accepted for new publication", name)
		}
	}
	for _, name := range []string{"widget", "@acme/widget", "@scope/_name", "@scope/-name", "widget.js"} {
		if !validPublishPackageName(name) {
			t.Errorf("ordinary publish name %q rejected", name)
		}
	}
}

func TestHistoricalNameParserRejectsUnsafeRoutes(t *testing.T) {
	for _, name := range []string{
		"", ".", "..", "-",
		"@scope/", "@scope/.", "@scope/..", "@scope/-", "@scope/../escape",
		"a/b", "@scope/name/extra", "bad%2Fname", "bad?name", "bad#name", "bad\\name", "bad\nname",
	} {
		if _, ok := parsePath(name); ok {
			t.Errorf("unsafe packument route %q accepted", name)
		}
	}
}
