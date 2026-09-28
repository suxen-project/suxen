package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
	_ "github.com/suxen-project/suxen/plugins/format/maven"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

type unsafeDirectoryRetentionFormat struct{}

func (unsafeDirectoryRetentionFormat) Name() string { return "unsafe-directory-retention-test" }
func (unsafeDirectoryRetentionFormat) RetentionUnitDirectory(_ spiformat.Repository, assetPath string) string {
	if strings.HasSuffix(assetPath, "/traversal") {
		return "../escape"
	}
	return "pkg/other/version"
}

func init() { spiformat.Register(unsafeDirectoryRetentionFormat{}) }

func TestCleanupRejectsUnsafeDirectoryDeclarations(t *testing.T) {
	f := newServerFixture(t)
	ctx := context.Background()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "unsafe-directory", Format: "unsafe-directory-retention-test", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"pkg/widget/version/traversal", "pkg/widget/version/wrong-parent"} {
		if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "unsafe-directory", Path: path, Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	policy := domain.CleanupPolicy{Name: "sweep", Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}}}
	result, err := f.Handler.cleanupRepository(ctx, policy, "unsafe-directory", false, time.Now())
	if err != nil || result.Deleted != 0 {
		t.Fatalf("unsafe declaration cleanup = %+v, %v", result, err)
	}
	for _, path := range []string{"pkg/widget/version/traversal", "pkg/widget/version/wrong-parent"} {
		if _, err := f.Metadata.Asset(ctx, "unsafe-directory", path); err != nil {
			t.Errorf("unsafe declaration removed %s: %v", path, err)
		}
	}
}

func putMavenRetentionAsset(t *testing.T, metadata *store.SQLStore, version, extension string) domain.Asset {
	t.Helper()
	suffix := "." + extension
	if strings.HasPrefix(extension, "-") {
		suffix = extension
	}
	asset, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "maven-retention", Path: fmt.Sprintf("org/example/widget/%s/widget-%s%s", version, version, suffix),
		Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func newMavenRetentionFixture(t *testing.T, repositoryType string) *serverFixture {
	t.Helper()
	f := newServerFixture(t)
	if err := f.Metadata.CreateRepository(context.Background(), domain.Repository{Name: "maven-retention", Format: "maven", Type: repositoryType, Upstream: "https://proxy.example"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMavenCleanupKeepsWholeLatestVersion(t *testing.T) {
	for _, repositoryType := range []string{"hosted", "proxy"} {
		t.Run(repositoryType, func(t *testing.T) {
			f := newMavenRetentionFixture(t, repositoryType)
			ctx := context.Background()
			for _, version := range []string{"1.0", "2.0"} {
				for _, ext := range []string{"pom", "jar", "jar.sha1", "-sources.jar", "-sources.jar.asc"} {
					putMavenRetentionAsset(t, f.Metadata, version, ext)
				}
			}
			policy := domain.CleanupPolicy{Name: "keep-one", KeepLast: 1, Criteria: domain.CleanupCriteria{{Path: "maven.artifactId", Op: "=", Value: "widget"}}}
			preview, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", true, time.Now())
			if err != nil || preview.Matched != 5 || len(preview.WouldDelete) != 5 {
				t.Fatalf("preview = %+v, %v", preview, err)
			}
			result, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", false, time.Now())
			if err != nil || result.Deleted != 5 {
				t.Fatalf("cleanup = %+v, %v", result, err)
			}
			for _, ext := range []string{"pom", "jar", "jar.sha1", "-sources.jar", "-sources.jar.asc"} {
				suffix := "." + ext
				if strings.HasPrefix(ext, "-") {
					suffix = ext
				}
				old := fmt.Sprintf("org/example/widget/1.0/widget-1.0%s", suffix)
				latest := fmt.Sprintf("org/example/widget/2.0/widget-2.0%s", suffix)
				if _, err := f.Metadata.Asset(ctx, "maven-retention", old); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("old %s survived: %v", old, err)
				}
				if _, err := f.Metadata.Asset(ctx, "maven-retention", latest); err != nil {
					t.Errorf("latest %s lost: %v", latest, err)
				}
			}
		})
	}
}

func TestMavenCleanupKeepsUnrelatedArtifactIndexEndingInSnapshot(t *testing.T) {
	f := newMavenRetentionFixture(t, "hosted")
	ctx := context.Background()
	index := "org/example/widget-SNAPSHOT/maven-metadata.xml"
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "maven-retention", Path: index, Kind: "raw", Digest: "index-digest"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "maven-retention", Path: "org/example/1.0/example-1.0.jar", Kind: "raw", Digest: "artifact-digest"}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{Name: "keep-one", KeepLast: 1, Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}}}
	result, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", false, time.Now())
	if err != nil || result.Deleted != 0 {
		t.Fatalf("unrelated artifact index selected for deletion: %+v, err=%v", result, err)
	}
	if _, err := f.Metadata.Asset(ctx, "maven-retention", index); err != nil {
		t.Fatalf("artifact index lost: %v", err)
	}
}

func TestMavenCleanupDeletesAnchoredSnapshotMetadataWithVersion(t *testing.T) {
	f := newMavenRetentionFixture(t, "hosted")
	ctx := context.Background()
	old := []string{
		"org/widget/1.0-SNAPSHOT/widget-1.0-20260807.120000-1.jar",
		"org/widget/1.0-SNAPSHOT/maven-metadata.xml",
	}
	for _, path := range old {
		if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "maven-retention", Path: path, Kind: "raw", Digest: "old-digest"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	newer := "org/widget/2.0/widget-2.0.jar"
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "maven-retention", Path: newer, Kind: "raw", Digest: "new-digest"}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{Name: "keep-one", KeepLast: 1, Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}}}
	result, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", false, time.Now())
	if err != nil || result.Deleted != len(old) {
		t.Fatalf("anchored snapshot unit cleanup: %+v, err=%v", result, err)
	}
	for _, path := range old {
		if _, err := f.Metadata.Asset(ctx, "maven-retention", path); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("old snapshot member %s survived: %v", path, err)
		}
	}
	if _, err := f.Metadata.Asset(ctx, "maven-retention", newer); err != nil {
		t.Fatalf("newer version lost: %v", err)
	}
}

func TestMavenCleanupPreservesVersionWhenPeerDoesNotMatch(t *testing.T) {
	f := newMavenRetentionFixture(t, "hosted")
	ctx := context.Background()
	for _, ext := range []string{"pom", "jar", "jar.sha1"} {
		putMavenRetentionAsset(t, f.Metadata, "1.0", ext)
	}
	peer, err := f.Metadata.Asset(ctx, "maven-retention", "org/example/widget/1.0/widget-1.0.jar.sha1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Metadata.SetAttributes(ctx, "maven-retention", peer.ID, "retention", map[string]any{"keep": true}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{Name: "sweep", Criteria: domain.CleanupCriteria{
		{Path: "maven.artifactId", Op: "=", Value: "widget"},
		{Path: "retention.keep", Op: "absent"},
	}}
	result, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", false, time.Now())
	if err != nil || result.Deleted != 0 {
		t.Fatalf("partial-match cleanup = %+v, %v", result, err)
	}
	for _, ext := range []string{"pom", "jar", "jar.sha1"} {
		path := fmt.Sprintf("org/example/widget/1.0/widget-1.0.%s", ext)
		if _, err := f.Metadata.Asset(ctx, "maven-retention", path); err != nil {
			t.Errorf("peer %s lost: %v", path, err)
		}
	}
}

func TestMavenCleanupNewPeerSkipsWholeDirectory(t *testing.T) {
	f := newMavenRetentionFixture(t, "hosted")
	ctx := context.Background()
	for _, ext := range []string{"pom", "jar"} {
		putMavenRetentionAsset(t, f.Metadata, "1.0", ext)
	}
	created := false
	f.Handler.metadata = &auditMavenUnitStore{Store: f.Metadata, beforeDelete: func() {
		if !created {
			created = true
			putMavenRetentionAsset(t, f.Metadata, "1.0", "jar.sha1")
		}
	}}
	policy := domain.CleanupPolicy{Name: "sweep", Criteria: domain.CleanupCriteria{{Path: "maven.artifactId", Op: "=", Value: "widget"}}}
	result, err := f.Handler.cleanupRepository(ctx, policy, "maven-retention", false, time.Now())
	if err != nil || result.Deleted != 0 || result.SkippedChanged != 2 || !created {
		t.Fatalf("concurrent cleanup = %+v, %v", result, err)
	}
	for _, ext := range []string{"pom", "jar", "jar.sha1"} {
		path := fmt.Sprintf("org/example/widget/1.0/widget-1.0.%s", ext)
		if _, err := f.Metadata.Asset(ctx, "maven-retention", path); err != nil {
			t.Errorf("peer %s lost: %v", path, err)
		}
	}
}

type auditMavenUnitStore struct {
	store.Store
	beforeDelete func()
}

func (s *auditMavenUnitStore) DeleteAssetsInDirectoryIfUnchanged(ctx context.Context, directory string, assets []domain.Asset) (bool, error) {
	s.beforeDelete()
	return s.Store.DeleteAssetsInDirectoryIfUnchanged(ctx, directory, assets)
}
