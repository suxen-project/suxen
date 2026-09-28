package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// retentionStubFormat models a format that owns retention grouping over paths
// shaped pkg/<name>/<version>/artifact, with a companion metadata document at
// pkg/<name>/<version>/meta.json. A "bad" package declares a traversing
// companion path to exercise the host's fail-closed validation.
type retentionStubFormat struct{}

func (retentionStubFormat) Name() string { return "retention-stub" }

func (retentionStubFormat) RetentionGroupKey(_ spiformat.Repository, asset spiformat.Asset) (string, bool) {
	if name, _, ok := stubCoordinate(asset.Path); ok {
		return "pkg/" + name, true
	}
	return "", false
}

func (retentionStubFormat) CompanionPaths(_ spiformat.Repository, assetPath string) []string {
	name, version, ok := stubCoordinate(assetPath)
	if !ok {
		return nil
	}
	if name == "bad" {
		return []string{"../escape.json"}
	}
	return []string{"pkg/" + name + "/" + version + "/meta.json"}
}

func stubCoordinate(assetPath string) (name, version string, ok bool) {
	parts := strings.Split(assetPath, "/")
	if len(parts) != 4 || parts[0] != "pkg" || parts[3] != "artifact" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func init() { spiformat.Register(retentionStubFormat{}) }

func TestCleanupCascadesCompanionsAndFailsClosed(t *testing.T) {
	t.Parallel()
	f := newServerFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "pkgs", Format: "retention-stub", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}

	type spec struct{ name, version string }
	for _, s := range []spec{{"alpha", "1.0.0"}, {"alpha", "2.0.0"}, {"bad", "1.0.0"}} {
		base := "pkg/" + s.name + "/" + s.version
		if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "pkgs", Path: base + "/artifact", Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "pkgs", Path: base + "/meta.json", Kind: "metadata", Digest: "sha256:" + strings.Repeat("b", 64), Size: 1}); err != nil {
			t.Fatal(err)
		}
	}

	policy := domain.CleanupPolicy{
		Name:         "sweep",
		Repositories: []string{"pkgs"},
		Criteria:     domain.CleanupCriteria{{Path: "sys.path", Op: "matches", Value: `/artifact$`}},
		Action:       "delete",
	}

	// Preview lists each artifact and the companions present in the snapshot.
	preview, err := f.Handler.cleanupRepository(ctx, policy, "pkgs", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(preview.WouldDelete, "pkg/alpha/1.0.0/artifact", "pkg/alpha/1.0.0/meta.json") {
		t.Fatalf("preview missing artifact+companion: %v", preview.WouldDelete)
	}

	result, err := f.Handler.cleanupRepository(ctx, policy, "pkgs", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (alpha 1.0.0 and 2.0.0 only): %+v", result.Deleted, result)
	}

	// Both alpha artifacts and their metadata companions cascaded away.
	for _, path := range []string{
		"pkg/alpha/1.0.0/artifact", "pkg/alpha/1.0.0/meta.json",
		"pkg/alpha/2.0.0/artifact", "pkg/alpha/2.0.0/meta.json",
	} {
		if _, err := f.Metadata.Asset(ctx, "pkgs", path); !isNotFound(err) {
			t.Fatalf("%q survived cleanup: %v", path, err)
		}
	}
	// The bad package declared an invalid companion path, so cleanup kept it.
	for _, path := range []string{"pkg/bad/1.0.0/artifact", "pkg/bad/1.0.0/meta.json"} {
		if _, err := f.Metadata.Asset(ctx, "pkgs", path); err != nil {
			t.Fatalf("fail-closed did not preserve %q: %v", path, err)
		}
	}
}

// A companion created concurrently, after cleanup selected the artifact but
// before the deletion commits, must still be removed with the artifact rather
// than orphaned.
func TestCleanupRemovesConcurrentlyCreatedCompanion(t *testing.T) {
	t.Parallel()
	f := newServerFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{Name: "pkgs", Format: "retention-stub", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	artifactPath := "pkg/gamma/1.0.0/artifact"
	companionPath := "pkg/gamma/1.0.0/meta.json"
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "pkgs", Path: artifactPath, Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1}); err != nil {
		t.Fatal(err)
	}

	created := false
	f.Handler.metadata = &auditCleanupMutationStore{Store: f.Metadata, beforeDelete: func(ctx context.Context, _ domain.Asset) error {
		if created {
			return nil
		}
		created = true
		// The companion appears only now — it was absent from cleanup's snapshot.
		_, err := f.Metadata.PutAsset(ctx, domain.Asset{Repository: "pkgs", Path: companionPath, Kind: "metadata", Digest: "sha256:" + strings.Repeat("b", 64), Size: 1})
		return err
	}}

	policy := domain.CleanupPolicy{
		Name:         "sweep",
		Repositories: []string{"pkgs"},
		Criteria:     domain.CleanupCriteria{{Path: "sys.path", Op: "matches", Value: `/artifact$`}},
		Action:       "delete",
	}
	result, err := f.Handler.cleanupRepository(ctx, policy, "pkgs", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1: %+v", result.Deleted, result)
	}
	if !created {
		t.Fatal("probe did not run: no delete was attempted")
	}
	if _, err := f.Metadata.Asset(ctx, "pkgs", companionPath); !isNotFound(err) {
		t.Fatalf("concurrently created companion was orphaned: %v", err)
	}
}

func containsAll(haystack []string, needles ...string) bool {
	present := make(map[string]struct{}, len(haystack))
	for _, item := range haystack {
		present[item] = struct{}{}
	}
	for _, needle := range needles {
		if _, ok := present[needle]; !ok {
			return false
		}
	}
	return true
}

func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}
