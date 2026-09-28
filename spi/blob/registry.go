package blob

import (
	"sort"
	"strings"
	"sync"
)

var registry = struct {
	sync.RWMutex
	drivers map[string]Driver
}{
	drivers: make(map[string]Driver),
}

// Register makes a blob storage driver available to the running binary. It
// panics for invalid or duplicate registrations because those are build
// errors, not runtime conditions.
func Register(driver Driver) {
	if err := driver.validate(); err != nil {
		panic("blob: " + err.Error())
	}

	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.drivers[driver.Name]; exists {
		panic("blob: register duplicate driver " + driver.Name)
	}
	if driver.URLScheme != "" {
		for _, existing := range registry.drivers {
			if existing.URLScheme == driver.URLScheme {
				panic("blob: URL scheme " + driver.URLScheme +
					" already claimed by driver " + existing.Name)
			}
		}
	}
	registry.drivers[driver.Name] = driver
}

// Lookup returns a registered driver by name.
func Lookup(name string) (Driver, bool) {
	registry.RLock()
	defer registry.RUnlock()
	driver, found := registry.drivers[name]
	return driver, found
}

// ForURL returns the registered driver claiming the URL scheme of a
// configuration string such as "s3://bucket/prefix".
func ForURL(configuration string) (Driver, bool) {
	scheme, _, found := strings.Cut(configuration, "://")
	if !found || scheme == "" {
		return Driver{}, false
	}
	registry.RLock()
	defer registry.RUnlock()
	for _, driver := range registry.drivers {
		if driver.URLScheme == scheme {
			return driver, true
		}
	}
	return Driver{}, false
}

// Names returns the sorted names of all registered drivers.
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	names := make([]string, 0, len(registry.drivers))
	for name := range registry.drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// URLSchemes returns the sorted URL schemes claimed by registered drivers.
func URLSchemes() []string {
	registry.RLock()
	defer registry.RUnlock()
	schemes := make([]string, 0, len(registry.drivers))
	for _, driver := range registry.drivers {
		if driver.URLScheme != "" {
			schemes = append(schemes, driver.URLScheme)
		}
	}
	sort.Strings(schemes)
	return schemes
}
