package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProvisionNumbersRemainExact(t *testing.T) {
	const integer = "18446744073709551617" // beyond uint64
	const decimal = "0.123456789012345678901234567890123456789"
	const exponent = "1.234567890123456789e+42"
	const oversizedExponent = "1e1000"
	valueCases := []struct{ name, literal string }{
		{"integer", integer}, {"decimal", decimal}, {"exponent", exponent}, {"oversized exponent", oversizedExponent},
	}
	parsers := []struct {
		name  string
		parse func(string) (Document, error)
		input func(string) string
	}{
		{"JSON", func(s string) (Document, error) { return Parse(strings.NewReader(s)) }, func(n string) string {
			return fmt.Sprintf(`{"resources":[{"kind":"downloadGate","name":"raw","spec":{"criteria":[{"path":"scan.serial","op":"=","value":%s}]}}]}`, n)
		}},
		{"YAML", func(s string) (Document, error) { return Parse(strings.NewReader(s)) }, func(n string) string {
			return fmt.Sprintf("resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.serial\n      op: '='\n      value: %s\n", n)
		}},
		{"JSONL", func(s string) (Document, error) { return Parse(strings.NewReader(s)) }, func(n string) string {
			return fmt.Sprintf(`{"kind":"downloadGate","name":"raw","spec":{"criteria":[{"path":"scan.serial","op":"=","value":%s}]}}`, n)
		}},
		{"canonical YAML", func(s string) (Document, error) { return ParseCanonical(strings.NewReader(s)) }, func(n string) string {
			return fmt.Sprintf("resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.serial\n      op: '='\n      value: %s\n", n)
		}},
		{"canonical JSON", func(s string) (Document, error) { return ParseCanonicalJSON(strings.NewReader(s)) }, func(n string) string {
			return fmt.Sprintf(`{"resources":[{"kind":"downloadGate","name":"raw","spec":{"criteria":[{"path":"scan.serial","op":"=","value":%s}]}}]}`, n)
		}},
	}
	for _, parser := range parsers {
		for _, value := range valueCases {
			t.Run(parser.name+"/"+value.name, func(t *testing.T) {
				document, err := parser.parse(parser.input(value.literal))
				if err != nil {
					t.Fatal(err)
				}
				criterion := document.Resources[0].Spec["criteria"].([]any)[0].(map[string]any)
				if _, ok := criterion["value"].(json.Number); !ok {
					t.Fatalf("number decoded as %T: %#v", criterion["value"], criterion["value"])
				}
				encoded, err := json.Marshal(document.Resources[0].Spec)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(encoded), value.literal) {
					t.Fatalf("number changed: %s", encoded)
				}
			})
		}
	}
}

func TestProvisionYAMLNumericSpellings(t *testing.T) {
	for _, test := range []struct{ input, expected string }{
		{"1.e3", "1.0e3"}, {".5e3", "0.5e3"}, {"01.25", "1.25"},
		{"01e3", "1e3"}, {"+01e3", "1e3"}, {"!!float 0x10", "16"},
		{"!!int 18446744073709551617", "18446744073709551617"},
	} {
		t.Run(test.input, func(t *testing.T) {
			input := "resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.score\n      op: '='\n      value: " + test.input + "\n"
			doc, err := Parse(strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(doc.Resources[0].Spec)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"value":`+test.expected) {
				t.Fatalf("number changed: %s", encoded)
			}
		})
	}
}

func TestProvisionYAMLMergeKeepsExactNumber(t *testing.T) {
	doc, err := Parse(strings.NewReader("resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - <<: &predicate {path: scan.serial, op: '=', value: 18446744073709551617}\n"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(doc.Resources[0].Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"value":18446744073709551617`) {
		t.Fatalf("merge changed value: %s", encoded)
	}
}

func TestProvisionRejectsNonStringSpecKey(t *testing.T) {
	_, err := Parse(strings.NewReader("resources:\n- kind: repository\n  name: raw\n  spec:\n    format: raw\n    type: hosted\n    formatConfig:\n      123: value\n"))
	if err == nil {
		t.Fatal("non-string spec key accepted")
	}
}

func TestProvisionYAMLSpecSafety(t *testing.T) {
	t.Run("duplicate nested key", func(t *testing.T) {
		input := "resources:\n- kind: repository\n  name: raw\n  spec:\n    format: raw\n    type: hosted\n    formatConfig:\n      setting: first\n      setting: second\n"
		if _, err := Parse(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate key error = %v", err)
		}
	})
	t.Run("duplicate merge key", func(t *testing.T) {
		input := "resources:\n- kind: repository\n  name: raw\n  spec:\n    format: raw\n    type: hosted\n    formatConfig:\n      <<: {first: one}\n      <<: {second: two}\n"
		if _, err := Parse(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate merge key error = %v", err)
		}
	})
	t.Run("explicit string tag", func(t *testing.T) {
		input := "resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.serial\n      op: '='\n      value: !!str 1e1000\n"
		doc, err := Parse(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		value := doc.Resources[0].Spec["criteria"].([]any)[0].(map[string]any)["value"]
		if value != "1e1000" {
			t.Fatalf("explicit string tag decoded as %#v", value)
		}
	})
	t.Run("alias expansion", func(t *testing.T) {
		var input strings.Builder
		input.WriteString("resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.serial\n      op: '='\n      value:\n        n0: &n0 [0]\n")
		for i := 1; i < 20; i++ {
			fmt.Fprintf(&input, "        n%d: &n%d [*n%d, *n%d]\n", i, i, i-1, i-1)
		}
		if _, err := Parse(strings.NewReader(input.String())); err == nil || !strings.Contains(err.Error(), "alias expansion") {
			t.Fatalf("alias expansion error = %v", err)
		}
	})
	t.Run("cross-resource scalar expansion", func(t *testing.T) {
		var input strings.Builder
		input.WriteString("resources:\n- kind: repository\n  name: first\n  spec:\n    format: raw\n    type: hosted\n    formatConfig:\n      blob: &blob '")
		input.WriteString(strings.Repeat("x", 65536))
		input.WriteString("'\n- kind: repository\n  name: second\n  spec:\n    format: raw\n    type: hosted\n    formatConfig:\n      blobs: [")
		for i := 0; i < 128; i++ {
			if i > 0 {
				input.WriteByte(',')
			}
			input.WriteString("*blob")
		}
		input.WriteString("]\n")
		if _, err := Parse(strings.NewReader(input.String())); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("cross-resource alias expansion error = %v", err)
		}
	})
}

func TestProvisionRejectsNonJSONValuesBeforeMutation(t *testing.T) {
	for _, literal := range []string{".nan", ".inf", "-.inf"} {
		for _, parser := range []struct {
			name  string
			parse func(io.Reader) (Document, error)
		}{
			{"local", Parse}, {"canonical", ParseCanonical},
		} {
			t.Run(parser.name+"/"+literal, func(t *testing.T) {
				input := "resources:\n- kind: repository\n  name: earlier\n  spec: {format: raw, type: hosted}\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.score\n      op: '<'\n      value: " + literal + "\n"
				_, err := parser.parse(strings.NewReader(input))
				if err == nil {
					t.Fatal("unencodable nested number accepted")
				}
			})
		}
	}

	ctx := context.Background()
	store := newProvisionTestStore(t)
	doc := Document{APIVersion: APIVersion, Resources: []Resource{
		{Kind: "repository", Name: "earlier", Spec: map[string]any{"format": "raw", "type": "hosted"}},
		{Kind: "downloadGate", Name: "raw", Spec: map[string]any{"enabled": true, "criteria": []any{map[string]any{"path": "scan.score", "op": "<", "value": map[string]any{"bad": json.Number("NaN")}}}}},
	}}
	if _, err := (Engine{Store: store}).Apply(ctx, doc, Options{}); err == nil {
		t.Fatal("direct document accepted invalid nested number")
	}
	if _, err := store.Repository(ctx, "earlier"); err != domain.ErrNotFound {
		t.Fatalf("earlier resource was mutated: %v", err)
	}
	if _, err := ResolveSecrets(doc, EnvironmentResolver{}); err == nil {
		t.Fatal("secret resolution accepted invalid nested number")
	}
	doc.Resources[1].Spec["criteria"] = []any{map[string]any{"path": "scan.score", "op": "<", "value": math.NaN()}}
	if _, err := (Engine{Store: store}).Apply(ctx, doc, Options{}); err == nil {
		t.Fatal("direct document accepted NaN")
	}
	if _, err := store.Repository(ctx, "earlier"); err != domain.ErrNotFound {
		t.Fatalf("earlier resource was mutated: %v", err)
	}
}

func TestProvisionOverlayKeepsExistingPredicatePrecision(t *testing.T) {
	ctx := context.Background()
	store := newProvisionTestStore(t)
	if err := store.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	const literal = "18446744073709551617"
	if err := store.SetDownloadGate(ctx, domain.DownloadGate{Repository: "raw", Criteria: []domain.Predicate{{Path: "scan.serial", Op: "=", Value: json.Number(literal)}}}); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(strings.NewReader("resources:\n- kind: downloadGate\n  name: raw\n  spec: {enabled: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := (Engine{Store: store}).Apply(ctx, doc, Options{})
	if err != nil || report.Failed() {
		t.Fatalf("apply: %+v %v", report, err)
	}
	gate, err := store.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if gate.Criteria[0].Value != json.Number(literal) {
		t.Fatalf("predicate changed to %#v", gate.Criteria[0].Value)
	}
}

func TestProvisionPersistsExactPredicateAndDetectsUnchanged(t *testing.T) {
	ctx := context.Background()
	for _, literal := range []string{"18446744073709551617", "0.123456789012345678901234567890123456789", "1.234567890123456789e+42"} {
		t.Run(literal, func(t *testing.T) {
			store := newProvisionTestStore(t)
			if err := store.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			input := fmt.Sprintf(`{"resources":[{"kind":"downloadGate","name":"raw","spec":{"enabled":true,"criteria":[{"path":"scan.serial","op":"=","value":%s}]}}]}`, literal)
			doc, err := Parse(strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{Store: store}
			for i := 0; i < 2; i++ {
				report, err := engine.Apply(ctx, doc, Options{})
				if err != nil || report.Failed() {
					t.Fatalf("apply %d: %+v %v", i, report, err)
				}
				if i == 1 && report.Results[0].Status != StatusUnchanged {
					t.Fatalf("second apply status = %s", report.Results[0].Status)
				}
			}
			gate, err := store.DownloadGate(ctx, "raw")
			if err != nil {
				t.Fatal(err)
			}
			if gate.Criteria[0].Value != json.Number(literal) {
				t.Fatalf("stored value changed to %#v", gate.Criteria[0].Value)
			}
		})
	}
}

func TestProvisionResolvedTransportPreservesExactNumber(t *testing.T) {
	const literal = "18446744073709551617"
	input := "resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    enabled: true\n    criteria:\n    - path: scan.serial\n      op: '='\n      value: " + literal + "\n"
	document, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveSecrets(document, EnvironmentResolver{})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalResolved(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(payload) {
		t.Fatalf("transport is not JSON: %s", payload)
	}
	received, err := ParseCanonicalJSON(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(received.Resources[0].Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"value":`+literal) {
		t.Fatalf("transport changed numeric predicate: %s", encoded)
	}
}

func TestProvisionChangingNumericPredicate(t *testing.T) {
	ctx := context.Background()
	store := newProvisionTestStore(t)
	if err := store.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: store}
	for _, literal := range []string{"18446744073709551617", "0.123456789012345678901234567890123456789"} {
		input := fmt.Sprintf(`{"resources":[{"kind":"downloadGate","name":"raw","spec":{"enabled":true,"criteria":[{"path":"scan.serial","op":"=","value":%s}]}}]}`, literal)
		doc, err := Parse(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		report, err := engine.Apply(ctx, doc, Options{})
		if err != nil || report.Failed() {
			t.Fatalf("apply: %+v %v", report, err)
		}
		gate, err := store.DownloadGate(ctx, "raw")
		if err != nil {
			t.Fatal(err)
		}
		if gate.Criteria[0].Value != json.Number(literal) {
			t.Fatalf("apply status %s kept value %#v, want %s", report.Results[0].Status, gate.Criteria[0].Value, literal)
		}
	}
}

func TestProvisionOverlayKeepsInternalFieldsAndDetachesNestedValues(t *testing.T) {
	current := domain.BlobStore{
		Name: "raw", Driver: "fs", PhysicalIdentity: "physical-id",
		Attributes: map[string]any{"nested": map[string]any{"items": []any{"before"}}},
	}
	desired := current
	if err := overlaySpec(map[string]any{"attributes": map[string]any{"nested": map[string]any{"items": []any{"after"}}}}, &desired); err != nil {
		t.Fatal(err)
	}
	if desired.PhysicalIdentity != "physical-id" {
		t.Fatalf("internal identity lost: %+v", desired)
	}
	if got := current.Attributes["nested"].(map[string]any)["items"].([]any)[0]; got != "before" {
		t.Fatalf("overlay mutated current: %#v", got)
	}
	if got := desired.Attributes["nested"].(map[string]any)["items"].([]any)[0]; got != "after" {
		t.Fatalf("overlay value = %#v", got)
	}
	// A later caller mutation must also be isolated from the previous state.
	desired.Attributes["nested"].(map[string]any)["items"].([]any)[0] = "changed again"
	if got := current.Attributes["nested"].(map[string]any)["items"].([]any)[0]; got != "before" {
		t.Fatalf("desired aliases current: %#v", got)
	}
}
