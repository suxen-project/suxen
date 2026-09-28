package format

import (
	"slices"
	"testing"
)

type namedFormat string

func (f namedFormat) Name() string { return string(f) }

func TestCoreFormatsAreAlwaysRegistered(t *testing.T) {
	for _, name := range []string{"raw", "oci"} {
		if !Registered(name) {
			t.Fatalf("core format %q is not registered", name)
		}
		if _, found := Lookup(name); found {
			t.Fatalf("Lookup(%q) returned a plugin for a core format", name)
		}
	}
	if Registered("registry-test-absent") {
		t.Fatal("unknown format reported as registered")
	}
}

// registerFormatForTest registers a format and removes it when the test ends so
// the global registry stays clean across repeated (-count) runs.
func registerFormatForTest(t *testing.T, f Format) {
	t.Helper()
	Register(f)
	t.Cleanup(func() {
		registry.Lock()
		delete(registry.formats, f.Name())
		registry.Unlock()
	})
}

func TestRegisterAndLookup(t *testing.T) {
	registerFormatForTest(t, namedFormat("registry-test-fmt"))
	if !Registered("registry-test-fmt") {
		t.Fatal("registered format not reported")
	}
	registered, found := Lookup("registry-test-fmt")
	if !found || registered.Name() != "registry-test-fmt" {
		t.Fatalf("Lookup = %v, %v", registered, found)
	}
	names := Names()
	for _, name := range []string{"raw", "oci", "registry-test-fmt"} {
		if !slices.Contains(names, name) {
			t.Fatalf("Names() = %v, missing %q", names, name)
		}
	}
}

func TestRegisterRejectsInvalidFormats(t *testing.T) {
	assertPanics(t, "nil format", func() { Register(nil) })
	assertPanics(t, "empty name", func() { Register(namedFormat("")) })
	assertPanics(t, "core name", func() { Register(namedFormat("oci")) })
	assertPanics(t, "uppercase name", func() { Register(namedFormat("Maven")) })

	registerFormatForTest(t, namedFormat("registry-test-dup"))
	assertPanics(t, "duplicate", func() { Register(namedFormat("registry-test-dup")) })
}

func assertPanics(t *testing.T, name string, callback func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: Register did not panic", name)
		}
	}()
	callback()
}
