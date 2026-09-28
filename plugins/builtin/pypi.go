//go:build !suxen_no_pypi

package builtin

// Excluded from the binary with -tags suxen_no_pypi.
import _ "github.com/suxen-project/suxen/plugins/format/pypi"
