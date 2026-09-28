//go:build suxen_integration

package gcs

import (
	"os"
	"testing"

	"github.com/suxen-project/suxen/spi/blob"
	"github.com/suxen-project/suxen/spi/blob/blobtest"
)

// TestGCSConformanceAgainstConfiguredBackend runs the conformance suite
// against a real bucket or emulator, for example:
//
//	SUXEN_TEST_GCS='gcs://suxen-test?endpoint=http://localhost:4443' go test ./plugins/blobstore/gcs/
func TestGCSConformanceAgainstConfiguredBackend(t *testing.T) {
	configURL := os.Getenv("SUXEN_TEST_GCS")
	if configURL == "" {
		t.Skip("SUXEN_TEST_GCS is not configured")
	}
	blobtest.RunStoreSuite(t, func(t *testing.T) blob.Store {
		store, err := open(configURL)
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}
