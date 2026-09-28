// Package blob implements the built-in blob storage drivers.
//
// The driver contract is the public SPI in spi/blob; the interfaces are
// aliased here so internal call sites and third-party drivers exchange
// identical types.
package blob

import (
	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// Store persists immutable blobs by digest. See spi/blob for the contract.
type Store = spiblob.Store

// UploadStore persists mutable, transient upload sessions alongside immutable
// blobs. See spi/blob for the contract.
type UploadStore = spiblob.UploadStore
