package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/store"
)

func TestProvenanceBlobReadUsesAssetPlacement(t *testing.T) {
	ctx := context.Background()
	metadata, err := store.OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name: "default", Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_TEST_PROVENANCE_STORE"},
		PhysicalIdentity: strings.Repeat("0", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "images", Format: "oci", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	repository, err := metadata.Repository(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	physical, err := blob.NewFS(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("signed payload")
	hash := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	if _, err := physical.Put(ctx, digest, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: repository.Name, RepositoryID: repository.ID,
		Path: ocimodel.BlobPath("app", digest), Digest: digest, Size: int64(len(payload)),
		BlobStore: "default", Kind: "oci-blob",
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(content.New(content.Options{Metadata: metadata, DefaultBlobStore: physical}))
	// The upload request may still hold a pre-migration repository snapshot.
	repository.BlobStore = "old-store"
	got, err := handler.readProvenanceBlob(ctx, repository, "app", digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("provenance payload = %q; want %q", got, payload)
	}
}

func TestProvenanceArtifactManifestRecognizesCosignOCI11Shape(t *testing.T) {
	subject := &ocimodel.Descriptor{Digest: "sha256:subject"}
	manifest := ocimodel.ManifestEnvelope{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config: &ocimodel.Descriptor{
			MediaType: cosignSignatureArtifactType,
			Digest:    "sha256:config",
		},
		Layers: []ocimodel.Descriptor{{
			MediaType: cosignSimpleSigningMediaType,
			Digest:    "sha256:payload",
			Annotations: map[string]string{
				cosignSignatureAnnotation: "base64-signature",
			},
		}},
		Subject: subject,
	}
	if !provenanceArtifactManifest(manifest) {
		t.Fatal("real Cosign OCI 1.1 config media type was not recognized")
	}

	manifest.ArtifactType = cosignSignatureArtifactType
	manifest.Config.MediaType = "application/vnd.oci.image.config.v1+json"
	manifest.Layers[0].MediaType = "application/vnd.oci.image.layer.v1.tar"
	delete(manifest.Layers[0].Annotations, cosignSignatureAnnotation)
	if provenanceArtifactManifest(manifest) {
		t.Fatal("forged Cosign artifactType accepted an ordinary image layer")
	}
}
