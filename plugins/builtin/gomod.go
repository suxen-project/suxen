//go:build !suxen_no_go

package builtin

// Excluded from the binary with -tags suxen_no_go.
import _ "github.com/suxen-project/suxen/plugins/format/gomod"
