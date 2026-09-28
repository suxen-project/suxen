//go:build !suxen_no_maven

package builtin

// Excluded from the binary with -tags suxen_no_maven.
import _ "github.com/suxen-project/suxen/plugins/format/maven"
