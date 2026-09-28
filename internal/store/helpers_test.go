package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func openMigrationTestSQLite(t *testing.T) *SQLStore {
	t.Helper()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := metadata.Close(); err != nil {
			t.Errorf("close SQLite metadata: %v", err)
		}
	})
	return metadata
}

func migrateTestMetadata(t *testing.T, metadata *SQLStore) {
	t.Helper()
	ctx := context.Background()
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := createTestDefaultBlobStore(ctx, metadata); err != nil {
		t.Fatal(err)
	}
}

func createTestDefaultBlobStore(ctx context.Context, metadata *SQLStore) error {
	return metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "default",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_BLOBSTORE",
		},
		PhysicalIdentity: strings.Repeat("0", 64),
	})
}

// createUploadTestRepository seeds the "registry" repository that
// testUploadSession targets. Upload sessions now carry a repository_id foreign
// key, so the repository must exist before a session references it.
func createUploadTestRepository(ctx context.Context, metadata *SQLStore) error {
	return metadata.CreateRepository(ctx, domain.Repository{
		Name:      "registry",
		Format:    "oci",
		Type:      "hosted",
		BlobStore: "default",
		Writable:  true,
	})
}

func openMigratedSQLite(t *testing.T) *SQLStore {
	t.Helper()
	metadata := openMigrationTestSQLite(t)
	migrateTestMetadata(t, metadata)
	return metadata
}
