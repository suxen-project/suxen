package server

import (
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/predicate"
)

func TestAssetPredicateOperatorsAndTypeSafety(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	attributes := map[string]any{
		"sys": map[string]any{
			"size":      int64(12),
			"updatedAt": now.Add(-31 * 24 * time.Hour),
			"path":      "nightly/package.zip",
		},
		"scan": map[string]any{
			"verified": true,
			"labels":   []any{"signed", "reviewed"},
			"result":   nil,
		},
		"vuln.trivy": map[string]any{
			"severity": "high",
		},
	}
	tests := []struct {
		name      string
		predicate domain.Predicate
		matches   bool
	}{
		{name: "numeric equality across JSON types", predicate: domain.Predicate{Path: "sys.size", Op: "=", Value: float64(12)}, matches: true},
		{name: "ordered number", predicate: domain.Predicate{Path: "sys.size", Op: ">", Value: 10}, matches: true},
		{name: "relative time", predicate: domain.Predicate{Path: "sys.updatedAt", Op: "before", Value: "30d"}, matches: true},
		{name: "absolute time", predicate: domain.Predicate{Path: "sys.updatedAt", Op: "after", Value: "2026-01-01T00:00:00Z"}, matches: true},
		{name: "regular expression", predicate: domain.Predicate{Path: "sys.path", Op: "matches", Value: `^nightly/`}, matches: true},
		{name: "string contains", predicate: domain.Predicate{Path: "sys.path", Op: "contains", Value: "package"}, matches: true},
		{name: "slice contains", predicate: domain.Predicate{Path: "scan.labels", Op: "contains", Value: "signed"}, matches: true},
		{name: "dotted namespace", predicate: domain.Predicate{Path: "vuln.trivy.severity", Op: "=", Value: "high"}, matches: true},
		{name: "in", predicate: domain.Predicate{Path: "sys.size", Op: "in", Value: []any{10.0, 12.0}}, matches: true},
		{name: "not in", predicate: domain.Predicate{Path: "sys.size", Op: "not-in", Value: []any{10.0, 11.0}}, matches: true},
		{name: "exists", predicate: domain.Predicate{Path: "scan.verified", Op: "exists"}, matches: true},
		{name: "explicit null equality", predicate: domain.Predicate{Path: "scan.result", Op: "=", Value: nil}, matches: true},
		{name: "missing null equality", predicate: domain.Predicate{Path: "scan.missing", Op: "=", Value: nil}, matches: false},
		{name: "absent", predicate: domain.Predicate{Path: "scan.missing", Op: "absent"}, matches: true},
		{name: "missing not in", predicate: domain.Predicate{Path: "scan.missing", Op: "not-in", Value: []any{"blocked"}}, matches: true},
		{name: "missing inequality does not match", predicate: domain.Predicate{Path: "scan.missing", Op: "!=", Value: "blocked"}, matches: false},
		{name: "inequality type mismatch does not match", predicate: domain.Predicate{Path: "sys.size", Op: "!=", Value: "12"}, matches: false},
		{name: "ordering type mismatch does not match", predicate: domain.Predicate{Path: "scan.verified", Op: ">", Value: 0}, matches: false},
		{name: "not in type mismatch does not match", predicate: domain.Predicate{Path: "sys.size", Op: "not-in", Value: []any{"12"}}, matches: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := predicate.Match(attributes, test.predicate, now); actual != test.matches {
				t.Fatalf("predicate.Match() = %t, want %t", actual, test.matches)
			}
		})
	}
}

func TestCleanupPredicatesSelectBlobStoreBeforeKeepLast(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	assets := []domain.Asset{
		{
			ID:        1,
			Path:      "old.bin",
			Kind:      "raw",
			UpdatedAt: now.Add(-72 * time.Hour),
			Attributes: map[string]any{
				"sys.blobStore": "forged-store",
			},
		},
		{ID: 2, Path: "new.bin", Kind: "raw", UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: 3, Path: "protected.bin", Kind: "raw", UpdatedAt: now.Add(-time.Hour)},
	}
	policy := domain.CleanupPolicy{
		Criteria: domain.CleanupCriteria{
			{Path: "sys.blobStore", Op: "=", Value: "space-constrained"},
			{Path: "sys.updatedAt", Op: "before", Value: "24h"},
		},
		KeepLast: 1,
	}
	candidates, err := selectCleanupCandidates(
		policy,
		domain.Repository{Name: "raw", Format: "raw", Type: "hosted", BlobStore: "space-constrained"},
		nil,
		assets,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != 1 {
		t.Fatalf("candidates = %+v, want only oldest matching asset", candidates)
	}

	policy.Criteria[0].Value = "other-store"
	candidates, err = selectCleanupCandidates(
		policy,
		domain.Repository{Name: "raw", Format: "raw", Type: "hosted", BlobStore: "space-constrained"},
		nil,
		assets,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("wrong blob store selected candidates: %+v", candidates)
	}
}
