package format

import (
	"sort"
	"sync"

	"github.com/suxen-project/suxen/spi/internal/ident"
)

var registry = struct {
	sync.RWMutex
	formats map[string]Format
}{
	formats: make(map[string]Format),
}

// coreFormats are implemented by the server itself rather than through this
// interface. Their names stay reserved so a plugin cannot shadow them.
var coreFormats = map[string]struct{}{
	"raw": {},
	"oci": {},
}

// Register makes a repository format available to the running binary. It
// panics for invalid or duplicate registrations because those are build
// errors, not runtime conditions.
func Register(f Format) {
	if f == nil || !ident.Valid(f.Name()) {
		panic("format: register requires a lowercase identifier")
	}
	name := f.Name()
	if _, core := coreFormats[name]; core {
		panic("format: " + name + " is a core format and cannot be registered")
	}

	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.formats[name]; exists {
		panic("format: register duplicate format " + name)
	}
	registry.formats[name] = f
}

// Lookup returns a registered format by name. Core formats ("raw", "oci") are
// not returned; they are handled by the server directly.
func Lookup(name string) (Format, bool) {
	registry.RLock()
	defer registry.RUnlock()
	f, found := registry.formats[name]
	return f, found
}

// Registered reports whether a format name is available in this binary,
// either as a core format or through a registered plugin.
func Registered(name string) bool {
	if _, core := coreFormats[name]; core {
		return true
	}
	registry.RLock()
	defer registry.RUnlock()
	_, found := registry.formats[name]
	return found
}

// Names returns the sorted names of all available formats, including the
// core formats.
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	names := make([]string, 0, len(registry.formats)+len(coreFormats))
	for name := range registry.formats {
		names = append(names, name)
	}
	for name := range coreFormats {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
