//go:build !suxen_no_gcs

package builtin

// Excluded from the binary with -tags suxen_no_gcs.
import _ "github.com/suxen-project/suxen/plugins/blobstore/gcs"
