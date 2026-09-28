package blob_test

import (
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	spiblob "github.com/suxen-project/suxen/spi/blob"
	"github.com/suxen-project/suxen/spi/blob/blobtest"
)

func TestFSConformance(t *testing.T) {
	blobtest.RunStoreSuite(t, func(t *testing.T) spiblob.Store {
		store, err := blob.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}
