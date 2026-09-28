package cargo_test

import (
	"slices"
	"testing"

	"github.com/suxen-project/suxen/plugins/format/cargo"
	"github.com/suxen-project/suxen/spi/format"
)

func TestCargoRetentionGrouping(t *testing.T) {
	f := cargo.Format{}
	repository := format.Repository{Name: "crates", Format: "cargo", Type: "hosted"}

	// A crate file groups by crate under its download prefix, case-folded, and
	// carries its per-version index entry as a companion.
	key, ok := f.RetentionGroupKey(repository, format.Asset{Path: "dl/Widget/1.0.0/download"})
	if !ok || key != "dl/widget" {
		t.Fatalf("RetentionGroupKey = %q, %v; want dl/widget, true", key, ok)
	}
	companions := f.CompanionPaths(repository, "dl/Widget/1.0.0/download")
	if !slices.Equal(companions, []string{"index-meta/widget/1.0.0.json", "index-claim/widget/1.0.0.txt"}) {
		t.Fatalf("CompanionPaths = %v", companions)
	}
	companions = f.CompanionPaths(repository, "dl/Widget/1.0.0+build.1/download")
	if !slices.Equal(companions, []string{"index-meta/widget/1.0.0+build.1.json", "index-claim/widget/1.0.0.txt", "index-meta/widget/1.0.0.json"}) {
		t.Fatalf("build metadata CompanionPaths = %v", companions)
	}

	// Two versions of one crate compete for keepLast together.
	first, _ := f.RetentionGroupKey(repository, format.Asset{Path: "dl/widget/1.0.0/download"})
	second, _ := f.RetentionGroupKey(repository, format.Asset{Path: "dl/widget/2.0.0/download"})
	if first != second {
		t.Fatalf("versions grouped apart: %q vs %q", first, second)
	}

	// Config and index paths are not crate files: no special grouping, no companion.
	if _, ok := f.RetentionGroupKey(repository, format.Asset{Path: "config.json"}); ok {
		t.Fatal("config.json claimed a retention group")
	}
	if got := f.CompanionPaths(repository, "config.json"); got != nil {
		t.Fatalf("config.json declared companions: %v", got)
	}
}
