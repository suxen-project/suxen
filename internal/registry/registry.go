// Package registry provides the compile-time extension point for metadata
// store drivers. Blob storage drivers register through the public SPI in
// spi/blob instead.
package registry

import (
	"fmt"
	"sync"

	"github.com/suxen-project/suxen/internal/store"
)

// StoreFactory constructs a configured metadata store driver.
type StoreFactory func(config string) (store.Store, error)

var metadataStores = struct {
	sync.RWMutex
	factories map[string]StoreFactory
}{
	factories: make(map[string]StoreFactory),
}

// RegisterStore makes a metadata store driver available to the running binary.
// It panics for duplicate names or nil factories because those are build errors.
func RegisterStore(name string, factory StoreFactory) {
	metadataStores.Lock()
	defer metadataStores.Unlock()

	if factory == nil {
		panic("register nil metadata store factory")
	}
	if _, exists := metadataStores.factories[name]; exists {
		panic("register duplicate metadata store " + name)
	}
	metadataStores.factories[name] = factory
}

// Store constructs a registered metadata store driver by name.
func Store(name, config string) (store.Store, error) {
	metadataStores.RLock()
	factory := metadataStores.factories[name]
	metadataStores.RUnlock()

	if factory == nil {
		return nil, fmt.Errorf("unknown metadata store driver %q", name)
	}
	return factory(config)
}
