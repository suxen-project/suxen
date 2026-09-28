package npm_test

import (
	"slices"
	"testing"

	"github.com/suxen-project/suxen/plugins/format/npm"
	"github.com/suxen-project/suxen/spi/format"
)

func TestNpmRetentionGrouping(t *testing.T) {
	f := npm.Format{}
	repository := format.Repository{Name: "npm", Format: "npm", Type: "hosted"}

	// A scoped tarball groups by package under its reserved "-" folder and
	// carries its per-version metadata document as a companion.
	key, ok := f.RetentionGroupKey(repository, format.Asset{Path: "@scope/pkg/-/pkg-1.0.0.tgz"})
	if !ok || key != "@scope/pkg/-" {
		t.Fatalf("RetentionGroupKey = %q, %v; want @scope/pkg/-, true", key, ok)
	}
	companions := f.CompanionPaths(repository, "@scope/pkg/-/pkg-1.0.0.tgz")
	if !slices.Equal(companions, []string{"@scope/pkg/-/metadata/1.0.0.json"}) {
		t.Fatalf("CompanionPaths = %v", companions)
	}

	// Unscoped packages group the same way.
	if key, _ := f.RetentionGroupKey(repository, format.Asset{Path: "lodash/-/lodash-4.17.0.tgz"}); key != "lodash/-" {
		t.Fatalf("unscoped group = %q", key)
	}

	// A packument path is not a tarball: no special grouping, no companion.
	if _, ok := f.RetentionGroupKey(repository, format.Asset{Path: "lodash"}); ok {
		t.Fatal("packument claimed a retention group")
	}
	if got := f.CompanionPaths(repository, "lodash"); got != nil {
		t.Fatalf("packument declared companions: %v", got)
	}
}
