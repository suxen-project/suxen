package memblob

import (
	"testing"

	"github.com/suxen-project/suxen/spi/blob"
	"github.com/suxen-project/suxen/spi/blob/blobtest"
)

// The exported conformance suite is the contract: an out-of-tree driver that
// passes it behaves like the built-in drivers.
func TestConformance(t *testing.T) {
	blobtest.RunStoreSuite(t, func(t *testing.T) blob.Store {
		driver, found := blob.Lookup("mem")
		if !found {
			t.Fatal("mem driver is not registered")
		}
		store, err := driver.Open("mem://conformance")
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}
