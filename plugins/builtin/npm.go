//go:build !suxen_no_npm

package builtin

// Excluded from the binary with -tags suxen_no_npm.
import _ "github.com/suxen-project/suxen/plugins/format/npm"
