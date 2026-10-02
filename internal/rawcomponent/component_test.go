package rawcomponent

import (
	"strings"
	"testing"
)

const versionDirectoryPattern = `^(?P<name>(models|tracks)/.+)/(?P<version>[0-9][^/]*)/[^/]+$`

func components(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return map[string]any{ConfigKey: list}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"unknown top-level field", map[string]any{"layout": "x"}, "unknown raw formatConfig field"},
		{"components not a list", map[string]any{ConfigKey: "x"}, "must be a list"},
		{"entry not an object", map[string]any{ConfigKey: []any{"x"}}, "must be an object"},
		{"unknown entry field", components(map[string]any{"pattern": versionDirectoryPattern, "keep": 1}), "unknown field"},
		{"missing pattern", components(map[string]any{"anchor": `\.glb$`}), "pattern must be"},
		{"invalid regex", components(map[string]any{"pattern": `(?P<name>[`}), "invalid pattern"},
		{"missing version group", components(map[string]any{"pattern": `^(?P<name>.+)/[^/]+$`}), "named groups name and version"},
		{"missing name group", components(map[string]any{"pattern": `^.+/(?P<version>[^/]+)/[^/]+$`}), "named groups name and version"},
		{"invalid anchor", components(map[string]any{"pattern": versionDirectoryPattern, "anchor": `[`}), "invalid anchor"},
		{"empty anchor", components(map[string]any{"pattern": versionDirectoryPattern, "anchor": ""}), "anchor must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse() error = %v, want %q", err, tc.want)
			}
		})
	}
	if rules, err := Parse(nil); err != nil || rules != nil {
		t.Fatalf("Parse(nil) = %v, %v", rules, err)
	}
}

func TestMatchDerivesDirectoryAndSingleFileVersions(t *testing.T) {
	rules, err := Parse(components(
		map[string]any{"pattern": versionDirectoryPattern, "anchor": `\.(glb|zip)$`},
		map[string]any{"pattern": `^(?P<name>client/alpha)/[^/]+/trackmaniac-(?P<version>[^/-]+)-[^/]+$`, "anchor": `\.zip$`},
		map[string]any{"pattern": `^(?P<name>client/.+)/(?P<version>[^/]*)\.bin$`},
		map[string]any{"pattern": `^(?P<name>app)/(?P<version>[^/]+)/.+$`},
		map[string]any{"pattern": `^tools/(?P<version>[^/]+)/(?P<name>[^/]+)$`},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want Match
	}{
		{"models/blocksets/core/0.2.0/core.glb", Match{Rule: 0, Name: "models/blocksets/core", Version: "0.2.0", Directory: "models/blocksets/core/0.2.0", Anchor: true}},
		{"models/blocksets/core/0.2.0/SHA256SUMS", Match{Name: "models/blocksets/core", Version: "0.2.0", Directory: "models/blocksets/core/0.2.0"}},
		{"client/alpha/linux/trackmaniac-1.4.0-x86_64.zip", Match{Rule: 1, Name: "client/alpha", Version: "1.4.0", Anchor: true}},
		{"client/alpha/windows/trackmaniac-1.4.0-x86_64.zip.sha256", Match{Rule: 1, Name: "client/alpha", Version: "1.4.0"}},
		// A version directory owns the files below its subdirectories too.
		{"app/2.0/docs/readme.txt", Match{Rule: 3, Name: "app", Version: "2.0", Directory: "app/2.0", Anchor: true}},
		// A name captured after the version leaves the version in the file
		// part, so each tool's version is its own unit, not a directory.
		{"tools/1.0/lint", Match{Rule: 4, Name: "lint", Version: "1.0", Anchor: true}},
		// Unmatched paths fall back to the implicit rule: parent directory and
		// file name, or the root component.
		{"models/readme.txt", Match{Rule: 5, Name: "models", Version: "readme.txt", Anchor: true, Implicit: true}},
		{"readme.txt", Match{Rule: 5, Name: RootComponent, Version: "readme.txt", Anchor: true, Implicit: true}},
		// An empty version capture does not match, so the asset falls back.
		{"client/beta/.bin", Match{Rule: 5, Name: "client/beta", Version: ".bin", Anchor: true, Implicit: true}},
	} {
		if got := rules.Match(tc.path); got != tc.want {
			t.Errorf("Match(%q) = %+v; want %+v", tc.path, got, tc.want)
		}
	}
}

func TestMatchWithoutRulesUsesDirectoryAndFileName(t *testing.T) {
	got := Rules(nil).Match("dist/app-1.2.3.zip")
	want := Match{Rule: 0, Name: "dist", Version: "app-1.2.3.zip", Anchor: true, Implicit: true}
	if got != want {
		t.Fatalf("Match() = %+v; want %+v", got, want)
	}
	if left, right := Rules(nil).Match("dist/a.zip").Unit(), Rules(nil).Match("dist/b.zip").Unit(); left == right {
		t.Fatal("every file without a rule must be its own version")
	}
}

func TestMatchUsesFirstMatchingRule(t *testing.T) {
	rules, err := Parse(components(
		map[string]any{"pattern": `^(?P<name>a)/(?P<version>[^/]+)/[^/]+$`},
		map[string]any{"pattern": `^(?P<name>a/[^/]+)/(?P<version>[^/]+)$`},
	))
	if err != nil {
		t.Fatal(err)
	}
	got := rules.Match("a/1/file")
	if got.Rule != 0 || got.Name != "a" || got.Version != "1" || got.Directory != "a/1" {
		t.Fatalf("Match() = %+v", got)
	}
}

func TestMatchRequiresVersionToBeWholeSegmentForDirectoryUnits(t *testing.T) {
	rules, err := Parse(components(map[string]any{"pattern": `^(?P<name>pkg)/v(?P<version>[^/]+)/[^/]+$`}))
	if err != nil {
		t.Fatal(err)
	}
	got := rules.Match("pkg/v1.0/file")
	if got.Rule != 0 || got.Version != "1.0" || got.Directory != "" {
		t.Fatalf("Match() = %+v; want a file-named version", got)
	}
}

func TestMatchSkipsOverlappingCaptures(t *testing.T) {
	rules, err := Parse(components(map[string]any{"pattern": `^(?P<name>dist/app-(?P<version>[^/]+))\.zip$`}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rules.Match("dist/app-1.0.zip"); !got.Implicit || got.Name != "dist" || got.Version != "app-1.0.zip" {
		t.Fatalf("Match() = %+v; want the implicit rule", got)
	}
}
