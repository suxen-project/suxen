package npm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestJSONEncodedSizeMatchesMarshal(t *testing.T) {
	for _, input := range []string{
		`null`, `true`, `false`, `0`, `1.2e30`, `""`, `"<>&\u2028\u2029"`,
		`{"":[],"nested":{"text":"café☃","nil":null,"flag":false},"array":[1,"\\",{},[]]}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(input), &value); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := jsonEncodedSize(value, int64(len(encoded)))
		if err != nil || got != int64(len(encoded)) {
			t.Fatalf("%s: counted %d bytes, want %d: %v", input, got, len(encoded), err)
		}
		if _, err := jsonEncodedSize(value, int64(len(encoded)-1)); !errors.Is(err, errPackumentBudget) {
			t.Fatalf("%s: one-byte-short budget returned %v", input, err)
		}
	}
	for _, value := range []any{map[string]any(nil), []any(nil), map[string]any{"map": map[string]any(nil), "slice": []any(nil)}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := jsonEncodedSize(value, int64(len(encoded)))
		if err != nil || got != int64(len(encoded)) {
			t.Fatalf("typed nil %T: counted %d bytes, want %d: %v", value, got, len(encoded), err)
		}
	}
}

func TestRewriteIndexRejectsRepeatedLongHostBeforeMarshal(t *testing.T) {
	versions := make(map[string]any)
	for index := range 8 {
		versions[fmt.Sprintf("1.0.%d", index)] = map[string]any{
			"dist": map[string]any{"tarball": "https://upstream.example/archive.tgz"},
		}
	}
	input, err := json.Marshal(map[string]any{"name": "widget", "versions": versions})
	if err != nil {
		t.Fatal(err)
	}
	const limit = 2048
	short, _, err := rewriteIndexWithLimit("widget", input, "application/json", "https://mirror.example/repository/npm", limit)
	if err != nil || len(short) > limit {
		t.Fatalf("short host rewrite = %d bytes: %v", len(short), err)
	}
	if _, _, err := rewriteIndexWithLimit("widget", input, "application/json", "https://"+strings.Repeat("h", 400)+"/repository/npm", limit); !errors.Is(err, errPackumentBudget) {
		t.Fatalf("long host rewrite = %v, want budget error", err)
	}
}

func TestRewriteBudgetIsIndependentOfVersionIterationOrder(t *testing.T) {
	versions := map[string]any{
		"1.0.0": map[string]any{"dist": map[string]any{"tarball": "https://up.example/a.tgz"}},
		"2.0.0": map[string]any{"dist": map[string]any{"tarball": "https://up.example/" + strings.Repeat("x", 220) + ".tgz"}},
	}
	input, err := json.Marshal(map[string]any{"versions": versions})
	if err != nil {
		t.Fatal(err)
	}
	repositoryURL := "https://" + strings.Repeat("h", 100) + "/repository/npm"
	want, _, err := rewriteIndexWithLimit("widget", input, "application/json", repositoryURL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) >= len(want) {
		t.Fatalf("test needs a net-growing rewrite: input %d, output %d", len(input), len(want))
	}
	for range 100 {
		got, _, err := rewriteIndexWithLimit("widget", input, "application/json", repositoryURL, int64(len(want)))
		if err != nil || string(got) != string(want) {
			t.Fatalf("exact-boundary rewrite failed: %v", err)
		}
	}
}

func TestHostedVersionBudgetStopsAccumulation(t *testing.T) {
	versions := map[string]any{}
	var used int64
	first := map[string]any{"dist": map[string]any{"tarball": "https://mirror.example/" + strings.Repeat("x", 80)}}
	if err := addHostedVersion(versions, "1.0.0", first, &used, 300); err != nil {
		t.Fatal(err)
	}
	second := map[string]any{"dist": map[string]any{"tarball": "https://mirror.example/" + strings.Repeat("x", 170)}}
	if err := addHostedVersion(versions, "2.0.0", second, &used, 300); !errors.Is(err, errPackumentBudget) {
		t.Fatalf("oversized second version = %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("oversized version was retained: %d entries", len(versions))
	}
}
