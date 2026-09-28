package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleDocumentsParse guards the committed example resource sets against
// schema drift: parsing validates every resource kind and spec field, so a
// renamed field breaks a unit test here instead of only when someone runs
// suxenctl apply against one of the examples.
func TestExampleDocumentsParse(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/repo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no example documents found under examples/*/repo.yaml")
	}
	for _, path := range matches {
		path := path
		t.Run(path, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			document, err := Parse(file)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			if len(document.Resources) == 0 {
				t.Fatalf("%s declares no resources", path)
			}
		})
	}
}
