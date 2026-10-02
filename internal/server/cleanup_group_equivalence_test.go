package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/gomod"
	_ "github.com/suxen-project/suxen/plugins/format/maven"
)

type equivalenceRepository struct {
	repository domain.Repository
	assets     []domain.Asset
}

func equivalenceAsset(path string) domain.Asset {
	return domain.Asset{Path: path, Kind: "raw", Digest: "sha256:" + strings.Repeat("e", 64), Size: 1}
}

func equivalenceManifest(image, tag string) domain.Asset {
	return domain.Asset{
		Path: "v2/" + image + "/manifests/" + tag, Kind: "oci-manifest", Reference: tag,
		Digest: "sha256:" + strings.Repeat("f", 64), Size: 1,
		ContentType: "application/vnd.oci.image.manifest.v1+json",
	}
}

func equivalenceFixtures() []equivalenceRepository {
	var maven []domain.Asset
	for _, version := range []string{"1.0", "1.1", "2.0"} {
		for _, suffix := range []string{".pom", ".jar", ".jar.sha1"} {
			maven = append(maven, equivalenceAsset("org/example/widget/"+version+"/widget-"+version+suffix))
		}
	}
	maven = append(maven, equivalenceAsset("org/example/widget/3.0/widget-3.0.pom"))
	var gomod []domain.Asset
	for _, version := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		for _, extension := range []string{"info", "mod", "zip"} {
			if version == "v1.2.0" && extension == "zip" {
				continue
			}
			gomod = append(gomod, equivalenceAsset("example.com/widget/@v/"+version+"."+extension))
		}
	}
	return []equivalenceRepository{
		{
			repository: domain.Repository{Name: "eq-raw-plain", Format: "raw", Type: "hosted"},
			assets: []domain.Asset{
				equivalenceAsset("root-a.txt"), equivalenceAsset("root-b.txt"),
				equivalenceAsset("docs/a.txt"), equivalenceAsset("docs/b.txt"), equivalenceAsset("docs/c.txt"),
				equivalenceAsset("docs/deep/x.txt"),
			},
		},
		{
			repository: domain.Repository{Name: "eq-raw-components", Format: "raw", Type: "hosted", FormatConfig: map[string]any{"components": []any{
				map[string]any{"pattern": `^(?P<name>models/.+)/(?P<version>[0-9][^/]*)/[^/]+$`, "anchor": `\.glb$`},
				map[string]any{"pattern": `^(?P<name>client/alpha)/[^/]+/trackmaniac-(?P<version>[^/-]+)-[^/]+$`, "anchor": `\.zip$`},
			}}},
			assets: []domain.Asset{
				equivalenceAsset("models/core/0.9.0/core.glb"), equivalenceAsset("models/core/0.9.0/SHA256SUMS"),
				equivalenceAsset("models/core/0.10.0/core.glb"), equivalenceAsset("models/core/0.10.0/SHA256SUMS"),
				equivalenceAsset("models/core/0.11.0/SHA256SUMS"),
				equivalenceAsset("models/core/0.12.0/core.glb"), equivalenceAsset("models/core/0.12.0/notes.md"),
				equivalenceAsset("client/alpha/linux/trackmaniac-11150-x86_64.zip"),
				equivalenceAsset("client/alpha/linux/trackmaniac-11150-x86_64.zip.sha256"),
				equivalenceAsset("client/alpha/windows/trackmaniac-11150-x86_64.zip"),
				equivalenceAsset("client/alpha/linux/trackmaniac-11151-x86_64.zip"),
				equivalenceAsset("client/alpha/windows/trackmaniac-11151-x86_64.zip"),
				equivalenceAsset("client/alpha/linux/trackmaniac-11160-x86_64.zip.sha256"),
				equivalenceAsset("client/alpha/linux/README"),
				equivalenceAsset("docs/a.txt"), equivalenceAsset("docs/b.txt"),
			},
		},
		{
			repository: domain.Repository{Name: "eq-oci", Format: "oci", Type: "hosted"},
			assets: []domain.Asset{
				equivalenceManifest("team/app", "1.9.0"), equivalenceManifest("team/app", "1.10.0"),
				equivalenceManifest("team/app", "latest"), equivalenceManifest("team/app", "sha256:"+strings.Repeat("1", 64)),
				equivalenceManifest("team/other", "1.0.0"),
			},
		},
		{repository: domain.Repository{Name: "eq-maven", Format: "maven", Type: "hosted"}, assets: maven},
		{repository: domain.Repository{Name: "eq-go", Format: "go", Type: "hosted"}, assets: gomod},
	}
}

func equivalencePolicies() []domain.CleanupPolicy {
	all := domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}}
	partial := domain.CleanupCriteria{{Path: "sys.path", Op: "matches", Value: `(\.glb|\.zip|\.jar|\.pom|\.txt|\.info|\.mod|/manifests/)`}}
	var policies []domain.CleanupPolicy
	for _, criteria := range []domain.CleanupCriteria{all, partial} {
		for _, keepLast := range []int{0, 1, 2} {
			for _, order := range []string{domain.CleanupOrderUpdatedAt, domain.CleanupOrderVersion} {
				policies = append(policies, domain.CleanupPolicy{Name: "eq", Criteria: criteria, KeepLast: keepLast, Order: order})
			}
		}
	}
	return policies
}

// selectionPaths flattens a whole-repository selection into its deleted paths.
func selectionPaths(selection cleanupSelection) []string {
	var paths []string
	for _, unit := range selection.directoryUnits {
		for _, asset := range unit.assets {
			paths = append(paths, asset.Path)
		}
	}
	for _, unit := range selection.units {
		for _, asset := range unit {
			paths = append(paths, asset.Path)
		}
	}
	for _, candidate := range selection.candidates {
		paths = append(paths, candidate.Path)
	}
	for _, version := range selection.rawVersions {
		for _, asset := range version.assets {
			paths = append(paths, asset.Path)
		}
	}
	slices.Sort(paths)
	return paths
}

func TestPerGroupCleanupMatchesWholeRepositorySelection(t *testing.T) {
	for _, fixture := range equivalenceFixtures() {
		partialSelections := 0
		for index, policy := range equivalencePolicies() {
			f := newServerFixture(t)
			ctx := context.Background()
			if err := f.Metadata.CreateRepository(ctx, fixture.repository); err != nil {
				t.Fatal(err)
			}
			for _, asset := range fixture.assets {
				asset.Repository = fixture.repository.Name
				if _, err := f.Metadata.PutAsset(ctx, asset); err != nil {
					t.Fatalf("%s %s: %v", fixture.repository.Name, asset.Path, err)
				}
				time.Sleep(time.Millisecond)
			}
			repository, err := f.Metadata.Repository(ctx, fixture.repository.Name)
			if err != nil {
				t.Fatal(err)
			}
			assets, err := f.Metadata.ForRepository(repository).Assets(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			whole, err := selectRepositoryCleanup(policy, repository, assets, now)
			if err != nil {
				t.Fatal(err)
			}
			want := selectionPaths(whole)
			if len(want) > 0 && len(want) < len(assets) {
				partialSelections++
			}
			preview, err := f.Handler.cleanupRepository(ctx, policy, repository.Name, true, now)
			if err != nil {
				t.Fatal(err)
			}
			got := append([]string(nil), preview.WouldDelete...)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("%s policy %d (%+v): per-group = %v, whole repository = %v", repository.Name, index, policy, got, want)
			}
			if preview.Scanned != len(assets) {
				t.Fatalf("%s scanned = %d, want %d", repository.Name, preview.Scanned, len(assets))
			}
			result, err := f.Handler.cleanupRepository(ctx, policy, repository.Name, false, now)
			if err != nil || result.Deleted != len(want) {
				t.Fatalf("%s policy %d deleted = %+v, %v; want %d", repository.Name, index, result, err, len(want))
			}
		}
		// Guard against a vacuous comparison: some policies must keep part of
		// the repository and delete the rest.
		if partialSelections == 0 {
			t.Fatalf("%s: no policy produced a partial selection", fixture.repository.Name)
		}
	}
}
