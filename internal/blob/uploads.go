package blob

import (
	"fmt"
	"math"
	"path"
	"strings"

	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// limitPlusOne lets bounded readers detect one excess byte without overflowing
// when the allowed size already reaches the largest representable int64 value.
func limitPlusOne(size int64) int64 {
	if size == math.MaxInt64 {
		return size
	}
	return size + 1
}

// ErrUploadTooLarge indicates that appending data would exceed an upload's size limit.
var ErrUploadTooLarge = spiblob.ErrUploadTooLarge

// ErrUploadExists indicates that an upload session already exists for a key.
var ErrUploadExists = spiblob.ErrUploadExists

// ErrUploadStoreUnsupported indicates that a blob driver cannot persist upload sessions.
var ErrUploadStoreUnsupported = spiblob.ErrUploadStoreUnsupported

func cleanUploadKey(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return "", fmt.Errorf("invalid upload key")
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid upload key")
	}
	return cleaned, nil
}
