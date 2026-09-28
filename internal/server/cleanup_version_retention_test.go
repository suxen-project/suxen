package server

import (
	"slices"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/plugins/format/cargo"
)

func TestCargoCleanupRetainsNewestMatchingVersionPerCrate(t *testing.T) {
	now := time.Now().UTC()
	var assets []domain.Asset
	for _, crate := range []string{"widget", "other"} {
		for index, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
			assets = append(assets, domain.Asset{
				ID: int64(len(assets) + 1), Path: "dl/" + crate + "/" + version + "/download",
				Kind: "raw", UpdatedAt: now.Add(time.Duration(index-4) * time.Hour),
			})
		}
	}
	policy := domain.CleanupPolicy{
		Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "matches", Value: `^dl/`}},
		KeepLast: 1,
	}
	candidates, err := selectCleanupCandidates(policy,
		domain.Repository{Name: "hosted", Format: "cargo", Type: "hosted"}, cargo.Format{}, assets, now)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, candidate := range candidates {
		paths = append(paths, candidate.Path)
	}
	want := []string{
		"dl/other/1.0.0/download", "dl/other/2.0.0/download",
		"dl/widget/1.0.0/download", "dl/widget/2.0.0/download",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("cleanup candidates = %v, want %v", paths, want)
	}
}
