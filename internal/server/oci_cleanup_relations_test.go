package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

func TestOCICleanupKeepsLastPerFullImageName(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	const repository = "nested-cleanup"
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: repository, Format: "oci", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	images := []string{"plain", "team/manifests/one", "team/manifests/two", "team/blobs/app"}
	for _, image := range images {
		for _, tag := range []string{"old", "latest"} {
			if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
				Repository: repository, Path: ocimodel.ManifestPath(image, tag),
				Kind: "oci-manifest", Reference: tag,
				Digest: "sha256:" + strings.Repeat("a", 64),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	policy := domain.CleanupPolicy{Name: "keep-one", KeepLast: 1, Action: "delete"}
	preview, err := fixture.Handler.cleanupRepository(ctx, policy, repository, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Scanned != 8 || len(preview.WouldDelete) != len(images) || preview.Deleted != 0 {
		t.Fatalf("preview = %+v", preview)
	}
	for _, image := range images {
		if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(image, "latest")); err != nil {
			t.Fatalf("preview removed %s: %v", image, err)
		}
	}
	result, err := fixture.Handler.cleanupRepository(ctx, policy, repository, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != len(images) {
		t.Fatalf("apply = %+v", result)
	}
	for _, image := range images {
		if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(image, "latest")); err != nil {
			t.Fatalf("latest tag for %s disappeared: %v", image, err)
		}
	}
}

func TestOCICleanupPrunesOnlyUnreferencedImageAlias(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	const repository = "nested-alias"
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: repository, Format: "oci", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	images := []string{"team/manifests/app", "team/manifests/other"}
	for _, image := range images {
		for _, reference := range []string{"latest", digest} {
			if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
				Repository: repository, Path: ocimodel.ManifestPath(image, reference),
				Kind: "oci-manifest", Reference: reference, Digest: digest,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	policy := domain.CleanupPolicy{
		Name: "remove-one", Action: "delete",
		Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "=", Value: ocimodel.ManifestPath(images[0], "latest")}},
	}
	preview, err := fixture.Handler.cleanupRepository(ctx, policy, repository, true, time.Now())
	if err != nil || len(preview.WouldDelete) != 1 || preview.Cascaded != 0 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	for _, image := range images {
		if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(image, digest)); err != nil {
			t.Fatalf("preview changed %s: %v", image, err)
		}
	}
	result, err := fixture.Handler.cleanupRepository(ctx, policy, repository, false, time.Now())
	if err != nil || result.Deleted != 1 || result.Cascaded != 1 {
		t.Fatalf("apply = %+v, %v", result, err)
	}
	if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(images[0], digest)); err == nil {
		t.Fatal("removed image's canonical alias remains")
	}
	for _, reference := range []string{"latest", digest} {
		if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(images[1], reference)); err != nil {
			t.Fatalf("other image %s changed: %v", reference, err)
		}
	}
}

func TestOCICleanupRetainsAliasWithInboundDependency(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	const repository = "dependent-alias"
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: repository, Format: "oci", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	base := "team/blobs/app"
	for _, reference := range []string{"latest", digest} {
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: repository, Path: ocimodel.ManifestPath(base, reference),
			Kind: "oci-manifest", Reference: reference, Digest: digest,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: repository, Path: ocimodel.ManifestPath("parent", "latest"),
		Kind: "oci-manifest", Reference: "latest", Digest: "sha256:" + strings.Repeat("d", 64),
		Dependencies: []string{digest},
	}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{
		Name: "remove-child-tag", Action: "delete",
		Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "=", Value: ocimodel.ManifestPath(base, "latest")}},
	}
	result, err := fixture.Handler.cleanupRepository(ctx, policy, repository, false, time.Now())
	if err != nil || result.Deleted != 1 || result.Cascaded != 0 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	if _, err := fixture.Metadata.Asset(ctx, repository, ocimodel.ManifestPath(base, digest)); err != nil {
		t.Fatalf("referenced alias removed: %v", err)
	}
}
