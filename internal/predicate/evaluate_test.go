package predicate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestMatchComparesLargeJSONNumbersExactly(t *testing.T) {
	attributes := map[string]any{"sys": map[string]any{"size": json.Number("9007199254740992")}}
	for _, trial := range []struct {
		op   string
		want bool
	}{
		{"=", false},
		{"<", true},
		{">", false},
	} {
		condition := domain.Predicate{Path: "sys.size", Op: trial.op, Value: json.Number("9007199254740993")}
		if got := Match(attributes, condition, time.Time{}); got != trial.want {
			t.Errorf("%s comparison matched = %t, want %t", trial.op, got, trial.want)
		}
	}
}

func TestValuesEqualJSONContainers(t *testing.T) {
	tests := []struct {
		name        string
		left, right any
		equal       bool
		comparable  bool
	}{
		{
			name:  "nested numeric spellings and representations",
			left:  map[string]any{"items": []any{map[string]any{"count": json.Number("9007199254740993")}, 1}},
			right: map[string]any{"items": []any{map[string]any{"count": json.Number("9.007199254740993e15")}, json.Number("1.0")}},
			equal: true, comparable: true,
		},
		{
			name:       "large adjacent numbers stay distinct",
			left:       []any{map[string]any{"count": json.Number("9007199254740993")}},
			right:      []any{map[string]any{"count": json.Number("9007199254740992")}},
			comparable: true,
		},
		{
			name: "array order matters",
			left: []any{1, 2}, right: []any{2, 1}, comparable: true,
		},
		{
			name: "array length matters",
			left: []any{1}, right: []any{1, 2}, comparable: true,
		},
		{
			name: "explicit null equals null",
			left: map[string]any{"value": nil}, right: map[string]any{"value": nil}, equal: true, comparable: true,
		},
		{
			name: "explicit null differs from missing key",
			left: map[string]any{"value": nil}, right: map[string]any{}, comparable: true,
		},
		{
			name: "nested number and string are incompatible",
			left: map[string]any{"value": 1}, right: map[string]any{"value": "1"},
		},
		{
			name: "incompatible child after different child fails closed",
			left: []any{1, 2}, right: []any{3, "2"},
		},
		{
			name: "incompatible shared child with different array length fails closed",
			left: []any{1, 2}, right: []any{"1"},
		},
		{
			name: "incompatible child beside missing key fails closed",
			left: map[string]any{"a": 1, "b": 2}, right: map[string]any{"a": "1", "c": 2},
		},
		{
			name: "null and object are incompatible",
			left: []any{nil}, right: []any{map[string]any{}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			equal, comparable := valuesEqual(test.left, test.right)
			if equal != test.equal || comparable != test.comparable {
				t.Fatalf("valuesEqual() = (%t, %t), want (%t, %t)", equal, comparable, test.equal, test.comparable)
			}
		})
	}
}

func TestMatchNestedJSONNumbersAcrossEqualityOperators(t *testing.T) {
	actual := map[string]any{"count": []any{json.Number("1e0"), map[string]any{"size": json.Number("9007199254740993")}}}
	matching := map[string]any{"count": []any{json.Number("1.0"), map[string]any{"size": json.Number("9.007199254740993e15")}}}
	other := map[string]any{"count": []any{json.Number("1"), map[string]any{"size": json.Number("9007199254740992")}}}
	attributes := map[string]any{"value": actual, "values": []any{other, actual}}
	for _, trial := range []struct {
		name  string
		path  string
		op    string
		value any
		want  bool
	}{
		{"equal", "value", "=", matching, true},
		{"not equal equivalent", "value", "!=", matching, false},
		{"not equal different", "value", "!=", other, true},
		{"contains", "values", "contains", matching, true},
		{"in", "value", "in", []any{other, matching}, true},
		{"not-in equivalent", "value", "not-in", []any{other, matching}, false},
		{"not-in different", "value", "not-in", []any{other}, true},
		{"not equal incompatible", "value", "!=", map[string]any{"count": "1"}, false},
		{"not-in incompatible", "value", "not-in", []any{map[string]any{"count": "1"}}, false},
	} {
		t.Run(trial.name, func(t *testing.T) {
			if got := Match(attributes, domain.Predicate{Path: trial.path, Op: trial.op, Value: trial.value}, time.Time{}); got != trial.want {
				t.Fatalf("Match() = %t, want %t", got, trial.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	created := now.Add(-48 * time.Hour)
	attributes := map[string]any{
		"sys": map[string]any{
			"size":         10,
			"blobStore":    "archive",
			"createdAt":    created,
			"lastAccessed": created.Format(time.RFC3339),
		},
		"classification": map[string]any{
			"label": "prod",
		},
		"tags":      []any{"alpha", "beta"},
		"summary":   "release candidate",
		"vuln.cvss": 7.5,
	}

	tests := []struct {
		name      string
		condition domain.Predicate
		want      bool
	}{
		{
			name:      "exists",
			condition: domain.Predicate{Path: "classification.label", Op: "exists"},
			want:      true,
		},
		{
			name:      "exists missing",
			condition: domain.Predicate{Path: "classification.missing", Op: "exists"},
			want:      false,
		},
		{
			name:      "absent",
			condition: domain.Predicate{Path: "classification.missing", Op: "absent"},
			want:      true,
		},
		{
			name:      "absent present",
			condition: domain.Predicate{Path: "classification.label", Op: "absent"},
			want:      false,
		},
		{
			name:      "missing path matches not-in",
			condition: domain.Predicate{Path: "classification.missing", Op: "not-in", Value: []any{"prod"}},
			want:      true,
		},
		{
			name:      "missing path does not match in",
			condition: domain.Predicate{Path: "classification.missing", Op: "in", Value: []any{"prod"}},
			want:      false,
		},
		{
			name:      "equal numbers",
			condition: domain.Predicate{Path: "sys.size", Op: "=", Value: 10},
			want:      true,
		},
		{
			name:      "equal number and float",
			condition: domain.Predicate{Path: "sys.size", Op: "=", Value: 10.0},
			want:      true,
		},
		{
			name:      "equal strings",
			condition: domain.Predicate{Path: "sys.blobStore", Op: "=", Value: "archive"},
			want:      true,
		},
		{
			name:      "number string type mismatch",
			condition: domain.Predicate{Path: "sys.size", Op: "=", Value: "10"},
			want:      false,
		},
		{
			name:      "not equal",
			condition: domain.Predicate{Path: "sys.blobStore", Op: "!=", Value: "default"},
			want:      true,
		},
		{
			name:      "not equal type mismatch",
			condition: domain.Predicate{Path: "sys.blobStore", Op: "!=", Value: 1},
			want:      false,
		},
		{
			name:      "less than",
			condition: domain.Predicate{Path: "sys.size", Op: "<", Value: 11},
			want:      true,
		},
		{
			name:      "less or equal",
			condition: domain.Predicate{Path: "sys.size", Op: "<=", Value: 10},
			want:      true,
		},
		{
			name:      "greater than",
			condition: domain.Predicate{Path: "sys.size", Op: ">", Value: 9},
			want:      true,
		},
		{
			name:      "greater or equal",
			condition: domain.Predicate{Path: "sys.size", Op: ">=", Value: 10},
			want:      true,
		},
		{
			name:      "ordered comparison type mismatch",
			condition: domain.Predicate{Path: "sys.size", Op: "<", Value: "10"},
			want:      false,
		},
		{
			name:      "before rfc3339",
			condition: domain.Predicate{Path: "sys.createdAt", Op: "before", Value: now.Format(time.RFC3339)},
			want:      true,
		},
		{
			name:      "after rfc3339",
			condition: domain.Predicate{Path: "sys.lastAccessed", Op: "after", Value: now.Add(-72 * time.Hour).Format(time.RFC3339)},
			want:      true,
		},
		{
			name:      "before retention duration",
			condition: domain.Predicate{Path: "sys.createdAt", Op: "before", Value: "1d"},
			want:      true,
		},
		{
			name:      "after retention duration",
			condition: domain.Predicate{Path: "sys.createdAt", Op: "after", Value: "1d"},
			want:      false,
		},
		{
			name:      "matches regexp",
			condition: domain.Predicate{Path: "summary", Op: "matches", Value: `release .*`},
			want:      true,
		},
		{
			name:      "matches invalid pattern fail-closed",
			condition: domain.Predicate{Path: "summary", Op: "matches", Value: `(`},
			want:      false,
		},
		{
			name:      "contains string",
			condition: domain.Predicate{Path: "summary", Op: "contains", Value: "candidate"},
			want:      true,
		},
		{
			name:      "contains slice",
			condition: domain.Predicate{Path: "tags", Op: "contains", Value: "beta"},
			want:      true,
		},
		{
			name:      "in",
			condition: domain.Predicate{Path: "classification.label", Op: "in", Value: []any{"prod", "dev"}},
			want:      true,
		},
		{
			name:      "not-in",
			condition: domain.Predicate{Path: "classification.label", Op: "not-in", Value: []any{"dev"}},
			want:      true,
		},
		{
			name:      "unknown operator fail-closed",
			condition: domain.Predicate{Path: "sys.size", Op: "approx", Value: 10},
			want:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Match(attributes, test.condition, now); got != test.want {
				t.Fatalf("Match(%q %s) = %v, want %v", test.condition.Path, test.condition.Op, got, test.want)
			}
		})
	}
}

func TestMatchAll(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	attributes := map[string]any{
		"sys": map[string]any{"size": 10},
	}
	if !MatchAll(attributes, nil, now) {
		t.Fatal("empty criteria should match")
	}
	if !MatchAll(attributes, []domain.Predicate{
		{Path: "sys.size", Op: "exists"},
		{Path: "sys.size", Op: ">=", Value: 10},
	}, now) {
		t.Fatal("conjunctive match unexpectedly failed")
	}
	if MatchAll(attributes, []domain.Predicate{
		{Path: "sys.size", Op: "exists"},
		{Path: "sys.size", Op: "<", Value: 5},
	}, now) {
		t.Fatal("conjunctive mismatch unexpectedly succeeded")
	}
}
