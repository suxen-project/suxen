package ident

import "testing"

func TestValid(t *testing.T) {
	for _, name := range []string{"gc", "cleanup", "blob-stores", "maven", "registry-test-fmt"} {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false", name)
		}
	}
	for _, name := range []string{"", "GC", "Gc", "_gc", "gc_job", "1gc", "-gc"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true", name)
		}
	}
}
