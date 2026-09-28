package predicate

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestInvalidNumericValuesFailClosed(t *testing.T) {
	for _, bad := range []any{json.Number("01"), json.Number("1/2"), json.Number("1e+"), json.Number("NaN"), math.NaN(), math.Inf(1), float32(math.Inf(-1))} {
		attributes := map[string]any{"value": bad, "values": []any{1, bad}}
		for _, trial := range []struct {
			path, op string
			value    any
		}{
			{"value", "=", bad},
			{"value", "!=", 2},
			{"value", "<", 2},
			{"value", ">=", 2},
			{"value", "in", []any{1, bad}},
			{"value", "not-in", []any{1, bad}},
			{"values", "contains", 1},
			{"values", "=", []any{1, bad}},
			{"values", "!=", []any{1, 2}},
			{"values", "not-in", []any{[]any{1, 2}}},
		} {
			if Match(attributes, domain.Predicate{Path: trial.path, Op: trial.op, Value: trial.value}, time.Time{}) {
				t.Errorf("invalid %v matched %s", bad, trial.op)
			}
		}
		if Match(map[string]any{"value": 2}, domain.Predicate{Path: "value", Op: "not-in", Value: []any{1, bad}}, time.Time{}) {
			t.Errorf("invalid choice %v matched not-in", bad)
		}
	}
	if Match(map[string]any{"value": []json.Number{"1", "bogus"}}, domain.Predicate{Path: "value", Op: "contains", Value: json.Number("1")}, time.Time{}) {
		t.Error("invalid number in typed slice matched contains")
	}
	missing := map[string]any{}
	if Match(missing, domain.Predicate{Path: "value", Op: "not-in", Value: []any{json.Number("1e+")}}, time.Time{}) {
		t.Error("missing path matched not-in with invalid numeric choice")
	}
	if !Match(missing, domain.Predicate{Path: "value", Op: "not-in", Value: []any{json.Number("1e1000000")}}, time.Time{}) {
		t.Error("missing path did not match not-in with valid numeric choice")
	}
}

func TestOverrangeNumbersAcrossOperators(t *testing.T) {
	actual := map[string]any{"nested": []any{json.Number("1e1000001"), map[string]any{"negative": json.Number("1e-1000001")}}}
	equivalent := map[string]any{"nested": []any{json.Number("10e1000000"), map[string]any{"negative": json.Number("0.1e-1000000")}}}
	attributes := map[string]any{"value": actual, "choices": []any{actual}}
	for _, trial := range []struct {
		path, op string
		value    any
		want     bool
	}{
		{"value", "=", equivalent, true},
		{"value", "!=", equivalent, false},
		{"value", "in", []any{equivalent}, true},
		{"value", "not-in", []any{equivalent}, false},
		{"choices", "contains", equivalent, true},
		{"number", "=", json.Number("10e1000000"), true},
		{"number", "<=", json.Number("10e1000000"), true},
		{"number", ">=", json.Number("10e1000000"), true},
		{"number", "<", json.Number("2e1000001"), true},
		{"number", ">", json.Number("9e100000"), true},
	} {
		attributes["number"] = json.Number("1e1000001")
		if got := Match(attributes, domain.Predicate{Path: trial.path, Op: trial.op, Value: trial.value}, time.Time{}); got != trial.want {
			t.Errorf("%s %s matched %t, want %t", trial.path, trial.op, got, trial.want)
		}
	}
}
