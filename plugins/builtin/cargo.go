//go:build !suxen_no_cargo

package builtin

// Excluded from the binary with -tags suxen_no_cargo.
import _ "github.com/suxen-project/suxen/plugins/format/cargo"
