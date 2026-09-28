package blob

import "testing"

func testDriver(name string, scheme string) Driver {
	return Driver{
		Name:      name,
		URLScheme: scheme,
		Open:      func(string) (Store, error) { return nil, nil },
	}
}

// registerForTest registers a driver and removes it when the test ends so the
// global registry stays clean across repeated (-count) runs.
func registerForTest(t *testing.T, driver Driver) {
	t.Helper()
	Register(driver)
	t.Cleanup(func() {
		registry.Lock()
		delete(registry.drivers, driver.Name)
		registry.Unlock()
	})
}

func TestRegisterAndLookup(t *testing.T) {
	registerForTest(t, testDriver("registry-test-alpha", "registry-test-alpha"))

	driver, found := Lookup("registry-test-alpha")
	if !found || driver.Name != "registry-test-alpha" {
		t.Fatalf("Lookup = %+v, %v", driver, found)
	}
	if _, found := Lookup("registry-test-unknown"); found {
		t.Fatal("Lookup returned an unregistered driver")
	}

	byURL, found := ForURL("registry-test-alpha://bucket/prefix")
	if !found || byURL.Name != "registry-test-alpha" {
		t.Fatalf("ForURL = %+v, %v", byURL, found)
	}
	if _, found := ForURL("registry-test-unknown://x"); found {
		t.Fatal("ForURL matched an unclaimed scheme")
	}
	if _, found := ForURL("no-scheme-at-all"); found {
		t.Fatal("ForURL matched a configuration without a scheme")
	}
}

func TestRegisterRejectsInvalidDrivers(t *testing.T) {
	assertPanics(t, "nil Open", func() {
		Register(Driver{Name: "registry-test-nil-open"})
	})
	assertPanics(t, "empty name", func() {
		Register(Driver{Open: func(string) (Store, error) { return nil, nil }})
	})
	for _, name := range []string{"Upper", "2start", "a_b", "a.b", "a/b", "a b"} {
		assertPanics(t, "invalid name "+name, func() {
			Register(testDriver(name, ""))
		})
	}
	for _, scheme := range []string{"Upper", "2start", "a_b", "a/b", "a b"} {
		assertPanics(t, "invalid scheme "+scheme, func() {
			Register(testDriver("registry-test-scheme", scheme))
		})
	}
	registerForTest(t, testDriver("registry-test-valid-scheme", "s3+https.v2"))

	registerForTest(t, testDriver("registry-test-dup", "registry-test-dup"))
	assertPanics(t, "duplicate name", func() {
		Register(testDriver("registry-test-dup", "registry-test-dup-2"))
	})
	assertPanics(t, "duplicate scheme", func() {
		Register(testDriver("registry-test-dup-other", "registry-test-dup"))
	})
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
