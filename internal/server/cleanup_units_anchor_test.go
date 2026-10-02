package server

import (
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

type anchoredTestDirectory struct{}

func (anchoredTestDirectory) RetentionUnitDirectory(_ spiformat.Repository, assetPath string) string {
	if strings.HasPrefix(assetPath, "pkg/v1/") {
		return "pkg/v1"
	}
	return ""
}

func (anchoredTestDirectory) IsRetentionUnitAnchor(_ spiformat.Repository, assetPath string) bool {
	return strings.Contains(assetPath, "/artifact-")
}

func (anchoredTestDirectory) RetentionGroupKey(_ spiformat.Repository, asset spiformat.Asset) (string, bool) {
	if strings.Contains(asset.Path, "other") {
		return "pkg/other", true
	}
	return "pkg/main", true
}

func (anchoredTestDirectory) CompanionPaths(_ spiformat.Repository, _ string) []string { return nil }

type legacyDirectoryOnly struct{}

func (legacyDirectoryOnly) RetentionUnitDirectory(_ spiformat.Repository, _ string) string {
	return "pkg/v1"
}

func TestAnchoredRetentionDirectoryRequiresConsistentArtifactsAndAllChildren(t *testing.T) {
	provider := anchoredTestDirectory{}
	repository := domain.Repository{Name: "pkg", Format: "anchored-test"}
	member := func(id int64, path string) domain.Asset {
		return domain.Asset{ID: id, Path: path, Kind: "raw", UpdatedAt: time.Unix(id, 0)}
	}
	all := domain.CleanupPolicy{Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}}}
	for _, tc := range []struct {
		name                    string
		policy                  domain.CleanupPolicy
		assets                  []domain.Asset
		wantUnits, wantOrdinary int
	}{
		{"metadata alone", all, []domain.Asset{member(1, "pkg/v1/meta.xml")}, 0, 1},
		{"anchored companions", all, []domain.Asset{member(1, "pkg/v1/artifact-a"), member(2, "pkg/v1/meta.xml")}, 1, 0},
		{"conflicting anchors", all, []domain.Asset{member(1, "pkg/v1/artifact-a"), member(2, "pkg/v1/artifact-other")}, 0, 0},
		{"partial criteria", domain.CleanupPolicy{Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "matches", Value: `artifact-`}}}, []domain.Asset{member(1, "pkg/v1/artifact-a"), member(2, "pkg/v1/meta.xml")}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units, ordinary := selectRetentionDirectoryUnits(tc.policy, repository, provider, provider, tc.assets, time.Now())
			if len(units) != tc.wantUnits || len(ordinary) != tc.wantOrdinary {
				t.Fatalf("units=%d, ordinary=%d, want %d/%d", len(units), len(ordinary), tc.wantUnits, tc.wantOrdinary)
			}
			if len(units) == 1 && len(units[0].assets) != len(tc.assets) {
				t.Fatalf("unit has %d members, want %d", len(units[0].assets), len(tc.assets))
			}
		})
	}
	// Existing providers that do not opt in keep their directory behavior.
	units, ordinary := selectRetentionDirectoryUnits(all, repository, nil, legacyDirectoryOnly{}, []domain.Asset{member(1, "pkg/v1/meta.xml")}, time.Now())
	if len(units) != 1 || len(ordinary) != 0 {
		t.Fatalf("legacy provider units=%d, ordinary=%d", len(units), len(ordinary))
	}
}
