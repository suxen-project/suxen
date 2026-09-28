package provision

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestE2EProvisionDocumentParses guards the interop-e2e provisioning fixture
// against schema drift: a rename in a resource spec used to surface only when
// the containers refused to boot mid-run. Parsing the embedded document here
// catches an unknown/renamed field in unit CI instead.
func TestE2EProvisionDocumentParses(t *testing.T) {
	raw, err := os.ReadFile("../../test/e2e/compose.e2e.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		XSuxen struct {
			Environment struct {
				Document string `yaml:"SUXEN_PROVISION_DOCUMENT"`
			} `yaml:"environment"`
		} `yaml:"x-suxen"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	document := compose.XSuxen.Environment.Document
	if strings.TrimSpace(document) == "" {
		t.Fatal("SUXEN_PROVISION_DOCUMENT not found in compose.e2e.yaml")
	}
	if _, err := Parse(strings.NewReader(document)); err != nil {
		t.Fatalf("e2e provisioning fixture does not parse: %v", err)
	}
}
