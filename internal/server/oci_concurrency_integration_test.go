//go:build suxen_integration

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestPostgresGCAndManifestPublicationAreAtomic(t *testing.T) {
	postgresURL := os.Getenv("SUXEN_TEST_POSTGRES")
	s3URL := os.Getenv("SUXEN_TEST_S3")
	if postgresURL == "" || s3URL == "" {
		t.Skip("SUXEN_TEST_POSTGRES and SUXEN_TEST_S3 are not configured")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	repositoryName := "oci-race-" + suffix
	storeName := "oci-race-store-" + suffix
	token := "oci-race-token-" + suffix
	metadata, err := store.OpenPostgres(postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewS3(s3URL)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "blobs")
	configuration := "tracking://" + storePath
	identity, err := content.PhysicalIdentity("tracking", configuration)
	if err != nil {
		t.Fatal(err)
	}
	environmentName := "SUXEN_OCI_RACE_STORE_" + suffix
	t.Setenv(environmentName, configuration)
	factory := func(driver string, value string) (blob.Store, error) {
		return blob.NewFS(strings.TrimPrefix(value, "tracking://"))
	}
	h := NewWithBlobStoreFactory(
		config.Config{
			BlobURL:           s3URL,
			BootstrapUser:     "oci-race-admin-" + suffix,
			BootstrapPassword: "oci-race-password-" + suffix,
			BootstrapToken:    token,
			MaxUploadBytes:    16 << 20,
		},
		metadata,
		defaultStore,
		factory,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	t.Cleanup(func() { _ = h.Close() })
	if _, err := h.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name: storeName, Driver: "tracking",
		ConfigurationRef: &domain.ConfigurationReference{Env: environmentName},
		PhysicalIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(context.Background(), domain.Repository{
		Name: repositoryName, Format: "oci", Type: "hosted", BlobStore: storeName,
	}); err != nil {
		t.Fatal(err)
	}
	assertGCAndManifestPublicationAreAtomic(t, h, repositoryName, storeName, token)
}
