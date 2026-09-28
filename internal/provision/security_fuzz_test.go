package provision

import (
	"bytes"
	"testing"
)

func FuzzProvisioningDocumentParsing(f *testing.F) {
	f.Add([]byte("apiVersion: suxen/v1\nresources: []\n"))
	f.Add([]byte(`{"apiVersion":"suxen/v1","resources":[]}`))
	f.Add([]byte("kind: Repository\nname: raw\nspec: {}\n"))
	f.Fuzz(func(t *testing.T, document []byte) {
		_, _ = Parse(bytes.NewReader(document))
		_, _ = ParseCanonical(bytes.NewReader(document))
		_, _ = ParseCanonicalJSON(bytes.NewReader(document))
	})
}
