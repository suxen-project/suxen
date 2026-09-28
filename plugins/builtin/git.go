//go:build !suxen_no_git

package builtin

// Excluded from the binary with -tags suxen_no_git.
import _ "github.com/suxen-project/suxen/plugins/format/git"
