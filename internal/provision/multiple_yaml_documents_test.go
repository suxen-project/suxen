package provision

import (
	"strings"
	"testing"
)

func TestProvisioningRejectsAdditionalYAMLDocuments(t *testing.T) {
	input := "apiVersion: suxen.io/v1\nresources: []\n---\napiVersion: suxen.io/v1\nresources:\n  - kind: role\n    name: reader\n"
	for name, parse := range map[string]func(*strings.Reader) (Document, error){
		"local":     func(input *strings.Reader) (Document, error) { return Parse(input) },
		"canonical": func(input *strings.Reader) (Document, error) { return ParseCanonical(input) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(strings.NewReader(input)); err == nil {
				t.Fatal("accepted a second YAML document that would be silently ignored")
			}
		})
	}
}
