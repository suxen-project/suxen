package npm

import (
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func FuzzPackumentMetadataRewrite(f *testing.F) {
	f.Add([]byte(`{"name":"demo","versions":{}}`))
	f.Add([]byte("not json"))
	f.Fuzz(func(t *testing.T, metadata []byte) {
		_, _, _ = (Format{}).RewriteIndex(
			format.Repository{Name: "npm", Format: "npm", Type: "proxy"},
			"demo", metadata, "application/json", "https://registry.example/repository/npm",
		)
	})
}
