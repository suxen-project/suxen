package pypi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRootJSONPreservesFieldsAndStableRendering(t *testing.T) {
	input := []byte(`{"z":{"retained":true},"meta":{"api-version":"1.0"},"projects":[{"name":"Widget","extension":7},{"name":"widget","extension":8},{"name":"Other"}],"a":1}`)
	page, err := parseRootPage(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.entries) != 2 {
		t.Fatalf("entries = %d", len(page.entries))
	}
	page.entries[0].url = "https://example.test/repository/pypi/simple/widget/"
	first, _, err := page.render()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := page.render()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("unstable render: %v", err)
	}
	var document struct {
		Z        map[string]bool  `json:"z"`
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	if !document.Z["retained"] || document.Projects[0]["extension"] != float64(7) || document.Projects[0]["url"] != page.entries[0].url {
		t.Fatalf("lost root extensions: %s", first)
	}
}

func TestRootJSONRejectsTrailingDataAndBoundsOutput(t *testing.T) {
	for _, input := range []string{`{"projects":[]} true`, `{"projects":[}`, `{"projects":[]}}`} {
		if _, err := parseRootPage([]byte(input)); err == nil {
			t.Fatalf("accepted invalid root %q", input)
		}
	}
	page := &rootPage{asJSON: true, fields: map[string]json.RawMessage{"large": json.RawMessage(`"` + strings.Repeat("x", rootRenderedLimit) + `"`)}}
	if _, _, err := page.render(); err == nil {
		t.Fatal("accepted oversized top-level field")
	}
}
