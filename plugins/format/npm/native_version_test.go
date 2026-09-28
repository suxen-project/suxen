package npm

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestNpmNativeVersionValidityOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/npm-semver-7.8.5-validity.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		version, expected, ok := strings.Cut(string(line), "\t")
		if !ok || expected != "true" && expected != "false" {
			t.Fatalf("invalid oracle row %q", line)
		}
		if got := validNpmVersion(version); got != (expected == "true") {
			t.Errorf("validNpmVersion(%q) = %v; npm semver 7.8.5 = %s", version, got, expected)
		}
	}
}

func TestNpmLatestIgnoresNativeInvalidVersions(t *testing.T) {
	invalid := "9007199254740992.0.0"
	versions := map[string]any{invalid: map[string]any{}, "1.0.0": map[string]any{}}
	if got := highestRelease(versions); got != "1.0.0" {
		t.Fatalf("hosted latest = %q, want 1.0.0", got)
	}
	first := []byte(`{"name":"widget","versions":{"1.0.0":{}},"dist-tags":{}}`)
	second := []byte(`{"name":"widget","versions":{"` + invalid + `":{}},"dist-tags":{}}`)
	merged, _, err := (Format{}).MergeGroupContent(format.Repository{}, "widget", [][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(merged, []byte(`"latest":"1.0.0"`)) || !bytes.Contains(merged, []byte(invalid)) {
		t.Fatalf("group lost valid latest or legacy version: %s", merged)
	}
}
