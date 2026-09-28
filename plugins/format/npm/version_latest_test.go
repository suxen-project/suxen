package npm_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/suxen-project/suxen/plugins/format/npm"
	"github.com/suxen-project/suxen/spi/format"
)

func TestGroupLatestStableForEqualPrecedenceVersions(t *testing.T) {
	for _, test := range []struct {
		name     string
		versions [2]string
		want     string
	}{
		{"release", [2]string{"1.0.0+one", "1.0.0+two"}, "1.0.0+two"},
		{"prerelease", [2]string{"1.0.0-rc.1+one", "1.0.0-rc.1+two"}, "1.0.0-rc.1+two"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := make([][]byte, 2)
			for i, version := range test.versions {
				sources[i] = []byte(fmt.Sprintf(`{"name":"widget","versions":{%q:{"name":"widget","version":%q}},"dist-tags":{}}`, version, version))
			}
			for _, ordered := range [][][]byte{sources, {sources[1], sources[0]}} {
				for i := 0; i < 20; i++ {
					body, _, err := (npm.Format{}).MergeGroupContent(format.Repository{}, "widget", ordered)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Tags map[string]string `json:"dist-tags"`
					}
					if err := json.Unmarshal(body, &result); err != nil {
						t.Fatal(err)
					}
					if got := result.Tags["latest"]; got != test.want {
						t.Fatalf("latest iteration %d = %q, want %q", i, got, test.want)
					}
				}
			}
		})
	}
}
