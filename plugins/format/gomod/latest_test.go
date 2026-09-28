package gomod

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestLatestVersionPreference(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions []string
		want     string
	}{
		{"release before prerelease", []string{"v1.2.0", "v1.3.0-rc.1"}, "v1.2.0"},
		{"prerelease before pseudo", []string{"v1.2.0-rc.1", "v1.3.1-0.20240101000000-abcdefabcdef"}, "v1.2.0-rc.1"},
		{"pseudo by commit time", []string{"v1.2.1-0.20230101000000-abcdefabcdef", "v0.0.0-20240101000000-123456789abc"}, "v0.0.0-20240101000000-123456789abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise both member orders: selection must not depend on which
			// repository or asset is encountered first.
			for order := range 2 {
				versions := []string{tc.versions[order], tc.versions[1-order]}
				assets := fakeAssets{content: make(map[string][]byte)}
				var sources [][]byte
				for _, version := range versions {
					path := "example.com/hello/@v/" + version + ".info"
					body, err := json.Marshal(versionInfo{Version: version})
					if err != nil {
						t.Fatal(err)
					}
					assets.paths = append(assets.paths, path,
						"example.com/hello/@v/"+version+".mod",
						"example.com/hello/@v/"+version+".zip")
					assets.content[path] = body
					sources = append(sources, body)
				}
				var f Format
				body, _, found, err := f.SynthesizeHosted(context.Background(), format.Repository{Type: "hosted"}, "example.com/hello/@latest", assets)
				if err != nil || !found {
					t.Fatalf("hosted latest: found=%v err=%v", found, err)
				}
				assertLatestVersion(t, body, tc.want)
				body, _, err = f.MergeGroupContent(format.Repository{Type: "group"}, "example.com/hello/@latest", sources)
				if err != nil {
					t.Fatal(err)
				}
				assertLatestVersion(t, body, tc.want)
			}
		})
	}
}

func assertLatestVersion(t *testing.T, body []byte, want string) {
	t.Helper()
	var info versionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != want {
		t.Errorf("latest = %q, want %q", info.Version, want)
	}
}
