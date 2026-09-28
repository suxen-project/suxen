// Command suxen-custom is a custom suxen distribution: the default plugin
// set plus the example in-memory blob store driver. This is the pattern the
// suxen-sdk image generates for out-of-tree plugins.
package main

import (
	_ "example.com/suxen-plugin-memblob"
	_ "example.com/suxen-plugin-memblob/exampleplugin"
	_ "github.com/suxen-project/suxen/plugins/builtin"

	"github.com/suxen-project/suxen/suxencmd"
)

func main() {
	suxencmd.Main()
}
