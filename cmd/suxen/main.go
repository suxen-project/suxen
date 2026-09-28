// Command suxen runs the artifact repository server with the default plugin
// set compiled in.
package main

import (
	_ "github.com/suxen-project/suxen/plugins/builtin"

	"github.com/suxen-project/suxen/suxencmd"
)

func main() {
	suxencmd.Main()
}
