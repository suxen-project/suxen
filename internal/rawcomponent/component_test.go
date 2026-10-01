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
		map[string]any{"pattern": `^(?P<name>client/alpha/[^/]+)/trackmaniac-(?P<version>[^/]+)-[^/]+\.zip$`},
		map[string]any{"pattern": `^(?P<name>client/.+)/(?P<version>[^/]*)\.bin$`},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want Match
		ok   bool
	}{
		{"models/blocksets/core/0.2.0/core.glb", Match{Name: "models/blocksets/core", Version: "0.2.0", Directory: "models/blocksets/core/0.2.0", Anchor: true}, true},
		{"models/blocksets/core/0.2.0/SHA256SUMS", Match{Name: "models/blocksets/core", Version: "0.2.0", Directory: "models/blocksets/core/0.2.0"}, true},
		{"client/alpha/linux/trackmaniac-1.4.0-x86_64.zip", Match{Name: "client/alpha/linux", Version: "1.4.0", Anchor: true}, true},
		{"models/readme.txt", Match{}, false},
		// An empty version capture does not match, so the asset keeps the defaults.
		{"client/beta/.bin", Match{}, false},
	} {
		got, ok := rules.Match(tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Match(%q) = %+v, %t; want %+v, %t", tc.path, got, ok, tc.want, tc.ok)
		}
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
	got, ok := rules.Match("a/1/file")
	if !ok || got.Name != "a" || got.Version != "1" || got.Directory != "a/1" {
		t.Fatalf("Match() = %+v, %t", got, ok)
	}
}

func TestMatchRequiresVersionToBeWholeParentSegmentForDirectoryUnits(t *testing.T) {
	rules, err := Parse(components(map[string]any{"pattern": `^(?P<name>pkg)/v(?P<version>[^/]+)/[^/]+$`}))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rules.Match("pkg/v1.0/file")
	if !ok || got.Version != "1.0" || got.Directory != "" {
		t.Fatalf("Match() = %+v, %t; want a single-file version", got, ok)
	}
}
