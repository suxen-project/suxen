package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// Both dialects must compare policy-relevant metadata in the same DELETE
// statement as the removal, including the nullable access timestamp.
func TestCleanupConditionalDeleteTracksMetadataOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		repository := fmt.Sprintf("cleanup-race-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: repository, Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"access", "retention", "classification", "validation", "unchanged"} {
			t.Run(change, func(t *testing.T) {
				asset, err := metadata.PutAsset(ctx, domain.Asset{
					Repository: repository, Path: change + ".bin", Kind: "raw",
					Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
					Attributes: map[string]any{"retention": map[string]any{"keep": false}},
				})
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "access":
					err = metadata.TouchAsset(ctx, asset.ID, time.Now().UTC())
				case "retention":
					err = metadata.SetAttributes(ctx, repository, asset.ID, "retention", map[string]any{"keep": true})
				case "classification":
					err = metadata.SetAttributes(ctx, repository, asset.ID, "classification", map[string]any{"stage": "stable"})
				case "validation":
					err = metadata.RefreshAsset(ctx, asset.ID, time.Now().UTC().Add(time.Minute))
				}
				if err != nil {
					t.Fatal(err)
				}
				deleted, err := metadata.DeleteAssetsIfUnchanged(ctx, []domain.Asset{asset})
				if err != nil || deleted != (change == "unchanged") {
					t.Fatalf("conditional delete after %s = %v, %v", change, deleted, err)
				}
				_, err = metadata.Asset(ctx, repository, asset.Path)
				if change == "unchanged" && !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("unchanged asset remains: %v", err)
				}
				if change != "unchanged" && err != nil {
					t.Fatalf("changed asset removed: %v", err)
				}
			})
		}
		artifact, err := metadata.PutAsset(ctx, domain.Asset{Repository: repository, Path: "artifact.tgz", Kind: "raw", Digest: "sha256:" + strings.Repeat("b", 64)})
		if err != nil {
			t.Fatal(err)
		}
		companion, err := metadata.PutAsset(ctx, domain.Asset{Repository: repository, Path: "metadata.json", Kind: "metadata", Digest: "sha256:" + strings.Repeat("c", 64)})
		if err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetAttributes(ctx, repository, companion.ID, "retention", map[string]any{"keep": true}); err != nil {
			t.Fatal(err)
		}
		deleted, err := metadata.DeleteAssetsIfUnchanged(ctx, []domain.Asset{artifact, companion})
		if err != nil || deleted {
			t.Fatalf("changed companion removed paired artifact: deleted=%v err=%v", deleted, err)
		}
		if _, err := metadata.Asset(ctx, repository, artifact.Path); err != nil {
			t.Fatalf("artifact removed with changed companion: %v", err)
		}
	})
}

func TestDirectoryConditionalDeleteChecksCompleteDirectChildrenOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		repository := fmt.Sprintf("directory-race-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: repository, Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		put := func(path string) domain.Asset {
			t.Helper()
			asset, err := metadata.PutAsset(ctx, domain.Asset{Repository: repository, Path: path, Kind: "raw", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1})
			if err != nil {
				t.Fatal(err)
			}
			return asset
		}
		for _, test := range []struct {
			name   string
			mutate func(string, domain.Asset)
		}{
			{"new-peer", func(directory string, _ domain.Asset) { put(directory + "/checksum.sha1") }},
			{"changed-peer", func(_ string, asset domain.Asset) {
				if err := metadata.SetAttributes(ctx, repository, asset.ID, "retention", map[string]any{"keep": true}); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				directory := "org/example/widget/" + test.name
				first, second := put(directory+"/pom"), put(directory+"/jar")
				test.mutate(directory, second)
				deleted, err := metadata.DeleteAssetsInDirectoryIfUnchanged(ctx, directory, []domain.Asset{first, second})
				if err != nil || deleted {
					t.Fatalf("changed directory deleted=%v err=%v", deleted, err)
				}
				for _, asset := range []domain.Asset{first, second} {
					if _, err := metadata.Asset(ctx, repository, asset.Path); err != nil {
						t.Errorf("peer %s lost: %v", asset.Path, err)
					}
				}
			})
		}
		directory := "org/example/widget/Case%_1"
		asset := put(directory + "/jar")
		put("org/example/widget/case%_1/jar")
		put(directory + "/nested/jar")
		deleted, err := metadata.DeleteAssetsInDirectoryIfUnchanged(ctx, directory, []domain.Asset{asset})
		if err != nil || !deleted {
			t.Fatalf("exact direct-child directory deletion=%v err=%v", deleted, err)
		}
		if _, err := metadata.Asset(ctx, repository, "org/example/widget/case%_1/jar"); err != nil {
			t.Errorf("differing-case path lost: %v", err)
		}
		if _, err := metadata.Asset(ctx, repository, directory+"/nested/jar"); err != nil {
			t.Errorf("nested path lost: %v", err)
		}
	})
}

func TestBlobStoreConfigurationHonorsUploadAndAssetReferencesOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		suffix := time.Now().UnixNano()
		name := fmt.Sprintf("metadata-integrity-store-%d", suffix)
		repositoryName := fmt.Sprintf("metadata-integrity-upload-%d", suffix)
		resource := domain.BlobStore{
			Name: name, Driver: "fs", PhysicalIdentity: fmt.Sprintf("%064x", suffix),
			ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_PASS5_SOURCE"},
		}
		if err := metadata.CreateBlobStore(ctx, resource); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: repositoryName, Format: "oci", Type: "hosted", BlobStore: name}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		session := UploadSession{
			UploadSessionIdentity: UploadSessionIdentity{
				ID: repositoryName, Repository: repositoryName, Image: "widget", BlobStore: name, Principal: "alice",
			},
			StorageKey: "oci/test/" + repositoryName, CreatedAt: now, UpdatedAt: now,
		}
		limits := UploadSessionLimits{MaxStagedBytes: 100, MaxPrincipalStagedBytes: 100, MaxPrincipalSessions: 2}
		repository, err := metadata.Repository(ctx, repositoryName)
		if err != nil {
			t.Fatal(err)
		}
		session.RepositoryID = repository.ID
		if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
			t.Fatal(err)
		}
		repository, err = metadata.Repository(ctx, repositoryName)
		if err != nil {
			t.Fatal(err)
		}
		repository.BlobStore = "default"
		if err := metadata.UpdateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
		resource, err = metadata.BlobStore(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		changed := resource
		changed.ConfigurationRef = &domain.ConfigurationReference{Env: "SUXEN_PASS5_REPLACEMENT"}
		// A definition change is rejected for every store regardless of its
		// references; only operational attributes stay editable.
		if err := metadata.UpdateBlobStore(ctx, changed); !errors.Is(err, domain.ErrBlobStoreDefinitionImmutable) {
			t.Fatalf("configuration change with upload = %v", err)
		}
		attributes := resource
		attributes.Attributes = map[string]any{"note": "safe"}
		if err := metadata.UpdateBlobStore(ctx, attributes); err != nil {
			t.Fatalf("attribute-only change with upload = %v", err)
		}
		if err := metadata.DeleteBlobStore(ctx, name, Ownership{}); !errors.Is(err, domain.ErrBlobStoreInUse) {
			t.Fatalf("delete store with upload = %v", err)
		}
		if err := metadata.DeleteUploadSession(ctx, session.ID, ""); err != nil {
			t.Fatal(err)
		}
		// Removing the upload does not make the definition editable.
		if err := metadata.UpdateBlobStore(ctx, changed); !errors.Is(err, domain.ErrBlobStoreDefinitionImmutable) {
			t.Fatalf("configuration change after upload cancellation = %v", err)
		}
		asset, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: repositoryName, Path: "v2/widget/blobs/legacy", Kind: "oci-blob",
			BlobStore: name, Digest: "sha256:" + strings.Repeat("d", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := metadata.UpdateBlobStore(ctx, changed); !errors.Is(err, domain.ErrBlobStoreDefinitionImmutable) {
			t.Fatalf("configuration change with physical asset = %v", err)
		}
		if err := metadata.DeleteBlobStore(ctx, name, Ownership{}); !errors.Is(err, domain.ErrBlobStoreInUse) {
			t.Fatalf("delete store with physical asset = %v", err)
		}
		if deleted, err := metadata.DeleteAssetIfUnchanged(ctx, asset.ID, asset.Digest, asset.UpdatedAt); err != nil || !deleted {
			t.Fatalf("delete physical asset = %v, %v", deleted, err)
		}
		// Even with every reference removed the definition stays fixed; the
		// unreferenced store can now be deleted instead of reconfigured.
		if err := metadata.UpdateBlobStore(ctx, changed); !errors.Is(err, domain.ErrBlobStoreDefinitionImmutable) {
			t.Fatalf("configuration change after references removed = %v", err)
		}
		if err := metadata.DeleteBlobStore(ctx, name, Ownership{}); err != nil {
			t.Fatalf("delete unreferenced store = %v", err)
		}
	})
}
