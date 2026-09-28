package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func createDrainTestBlobStore(t *testing.T, metadata *SQLStore, name, fill string) {
	t.Helper()
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:             name,
		Driver:           "memory",
		ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_TEST_" + strings.ToUpper(name)},
		PhysicalIdentity: strings.Repeat(fill, 64),
	}); err != nil {
		t.Fatalf("create blob store %q: %v", name, err)
	}
}

// A store selected as another store's drain target is a live write destination
// even with no repositories, uploads, or assets of its own, so it must not be
// deletable while the drain is active. Clearing the drain releases it.
func TestSQLiteDeleteRejectsActiveDrainTarget(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	createDrainTestBlobStore(t, metadata, "source", "1")
	createDrainTestBlobStore(t, metadata, "target", "2")

	if err := metadata.BeginBlobStoreDrain(ctx, "source", "target"); err != nil {
		t.Fatalf("BeginBlobStoreDrain: %v", err)
	}
	if err := metadata.DeleteBlobStore(ctx, "target", Ownership{}); !errors.Is(err, domain.ErrActiveDrainTarget) {
		t.Fatalf("DeleteBlobStore(active target) = %v, want ErrActiveDrainTarget", err)
	}
	if err := metadata.SetBlobStoreState(ctx, "source", domain.BlobStoreStateActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteBlobStore(ctx, "target", Ownership{}); err != nil {
		t.Fatalf("DeleteBlobStore(released target) = %v, want nil", err)
	}
}

// A completed drain no longer redirects writes. Its historical target name
// does not keep an otherwise unreferenced store alive.
func TestSQLiteDeleteAllowsUnreferencedDrainedTarget(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	createDrainTestBlobStore(t, metadata, "source", "1")
	createDrainTestBlobStore(t, metadata, "target", "2")
	if err := metadata.BeginBlobStoreDrain(ctx, "source", "target"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetBlobStoreState(ctx, "source", domain.BlobStoreStateDrained, "target"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteBlobStore(ctx, "target", Ownership{}); err != nil {
		t.Fatalf("DeleteBlobStore(unreferenced drained target) = %v, want nil", err)
	}
}

func TestSQLiteBeginBlobStoreDrainValidatesTarget(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	createDrainTestBlobStore(t, metadata, "source", "1")
	createDrainTestBlobStore(t, metadata, "target", "2")

	if err := metadata.BeginBlobStoreDrain(ctx, "source", "source"); !errors.Is(err, domain.ErrInvalidDrainTarget) {
		t.Fatalf("self target = %v, want ErrInvalidDrainTarget", err)
	}
	if err := metadata.BeginBlobStoreDrain(ctx, "source", "missing"); !errors.Is(err, domain.ErrInvalidDrainTarget) {
		t.Fatalf("missing target = %v, want ErrInvalidDrainTarget", err)
	}
	if err := metadata.SetBlobStoreState(ctx, "target", domain.BlobStoreStateDraining, "source"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.BeginBlobStoreDrain(ctx, "source", "target"); !errors.Is(err, domain.ErrInvalidDrainTarget) {
		t.Fatalf("non-active target = %v, want ErrInvalidDrainTarget", err)
	}
}

// Establishing a drain onto a target and deleting that target race on the same
// target row, so any interleaving must leave a consistent result: the source
// never ends up draining onto a store that no longer exists.
func TestSQLiteConcurrentDrainAndTargetDeletionStayConsistent(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	createDrainTestBlobStore(t, metadata, "source", "1")
	createDrainTestBlobStore(t, metadata, "target", "2")

	var wg sync.WaitGroup
	wg.Add(2)
	var drainErr, deleteErr error
	go func() {
		defer wg.Done()
		drainErr = metadata.BeginBlobStoreDrain(ctx, "source", "target")
	}()
	go func() {
		defer wg.Done()
		deleteErr = metadata.DeleteBlobStore(ctx, "target", Ownership{})
	}()
	wg.Wait()

	source, err := metadata.BlobStore(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	if source.DrainTarget != "" {
		if _, err := metadata.BlobStore(ctx, source.DrainTarget); err != nil {
			t.Fatalf("source drains onto %q which does not exist: %v (drainErr=%v deleteErr=%v)",
				source.DrainTarget, err, drainErr, deleteErr)
		}
	}
}

func TestSQLiteBlobStoreCRUDAndRepositoryReferences(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	archive := domain.BlobStore{
		Name:   "archive",
		Driver: "s3",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_ARCHIVE_BLOBSTORE",
		},
		PhysicalIdentity: strings.Repeat("a", 64),
		Attributes: map[string]any{
			"uploadSessions": map[string]any{"staleAfter": "30m"},
		},
	}
	if err := metadata.CreateBlobStore(ctx, archive); err != nil {
		t.Fatal(err)
	}
	created, err := metadata.BlobStore(ctx, archive.Name)
	if err != nil {
		t.Fatal(err)
	}
	if created.Driver != "s3" || created.ConfigurationRef == nil ||
		created.ConfigurationRef.Env != "SUXEN_ARCHIVE_BLOBSTORE" ||
		created.Attributes["uploadSessions"] == nil {
		t.Fatalf("created blob store = %+v", created)
	}

	// A blob store's definition is fixed at creation: changing driver,
	// configuration reference or physical destination is rejected even while
	// the store is unused, and the stored definition is left untouched.
	frozen := archive
	frozen.Driver = "fs"
	frozen.ConfigurationRef = &domain.ConfigurationReference{File: "/run/secrets/archive"}
	frozen.PhysicalIdentity = strings.Repeat("b", 64)
	if err := metadata.UpdateBlobStore(ctx, frozen); !errors.Is(
		err,
		domain.ErrBlobStoreDefinitionImmutable,
	) {
		t.Fatalf("unused definition change error = %v", err)
	}
	unchanged, err := metadata.BlobStore(ctx, archive.Name)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Driver != "s3" || unchanged.ConfigurationRef == nil ||
		unchanged.ConfigurationRef.Env != "SUXEN_ARCHIVE_BLOBSTORE" ||
		unchanged.PhysicalIdentity != strings.Repeat("a", 64) {
		t.Fatalf("definition changed despite immutability: %+v", unchanged)
	}

	// Operational attributes remain editable while unused.
	archive.Attributes = map[string]any{
		"uploadSessions": map[string]any{"staleAfter": "15m"},
	}
	if err := metadata.UpdateBlobStore(ctx, archive); err != nil {
		t.Fatalf("attribute update: %v", err)
	}

	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:      "archived",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: archive.Name,
	}); err != nil {
		t.Fatal(err)
	}
	// An unchanged definition is still a valid (idempotent) update, and
	// attributes remain editable once the store is referenced.
	if err := metadata.UpdateBlobStore(ctx, archive); err != nil {
		t.Fatalf("idempotent in-use update: %v", err)
	}
	archive.Attributes = map[string]any{
		"uploadSessions": map[string]any{"staleAfter": "5m"},
	}
	if err := metadata.UpdateBlobStore(ctx, archive); err != nil {
		t.Fatalf("in-use attribute update: %v", err)
	}
	updated, err := metadata.BlobStore(ctx, archive.Name)
	if err != nil {
		t.Fatal(err)
	}
	policy := updated.Attributes["uploadSessions"].(map[string]any)
	if policy["staleAfter"] != "5m" {
		t.Fatalf("updated blob store attributes = %+v", updated.Attributes)
	}
	// A definition change on a referenced store is rejected the same way.
	changedArchive := archive
	changedArchive.ConfigurationRef = &domain.ConfigurationReference{
		File: "/run/secrets/moved-archive",
	}
	changedArchive.PhysicalIdentity = strings.Repeat("c", 64)
	if err := metadata.UpdateBlobStore(ctx, changedArchive); !errors.Is(
		err,
		domain.ErrBlobStoreDefinitionImmutable,
	) {
		t.Fatalf("update referenced blob store error = %v", err)
	}
	if err := metadata.DeleteBlobStore(ctx, archive.Name, Ownership{}); !errors.Is(err, domain.ErrBlobStoreInUse) {
		t.Fatalf("delete referenced blob store error = %v", err)
	}
	if err := metadata.DeleteRepository(ctx, "archived", Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteBlobStore(ctx, archive.Name, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.BlobStore(ctx, archive.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get deleted blob store error = %v", err)
	}
}

func TestSQLiteRejectsDuplicatePhysicalBlobStoreIdentityAndDefaultDeletion(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	first := domain.BlobStore{
		Name:   "first",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_FIRST",
		},
		PhysicalIdentity: strings.Repeat("d", 64),
	}
	if err := metadata.CreateBlobStore(ctx, first); err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.Name = "duplicate"
	duplicate.ConfigurationRef = &domain.ConfigurationReference{Env: "SUXEN_DUPLICATE"}
	if err := metadata.CreateBlobStore(ctx, duplicate); !errors.Is(
		err,
		domain.ErrBlobStoreIdentityConflict,
	) {
		t.Fatalf("duplicate physical identity error = %v", err)
	}
	if err := metadata.DeleteBlobStore(ctx, "default", Ownership{}); !errors.Is(
		err,
		domain.ErrDefaultBlobStoreImmutable,
	) {
		t.Fatalf("delete default blob store error = %v", err)
	}
}

func TestSQLiteAllowsDefaultBlobStoreAttributeUpdatesOnly(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	configured, err := metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	configured.Attributes = map[string]any{
		"uploadSessions": map[string]any{"staleAfter": "10m"},
	}
	if err := metadata.UpdateBlobStore(ctx, configured); err != nil {
		t.Fatalf("update default blob store attributes: %v", err)
	}
	updated, err := metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	policy := updated.Attributes["uploadSessions"].(map[string]any)
	if policy["staleAfter"] != "10m" {
		t.Fatalf("default blob store attributes = %+v", updated.Attributes)
	}

	configured.PhysicalIdentity = strings.Repeat("f", 64)
	if err := metadata.UpdateBlobStore(ctx, configured); !errors.Is(
		err,
		domain.ErrBlobStoreDefinitionImmutable,
	) {
		t.Fatalf("default configuration update returned %v", err)
	}
}

func TestSQLiteRepositoryWithAssetsCannotChangeBlobStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("e", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "source",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "source",
		Path:       "artifact",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpdateRepository(ctx, domain.Repository{
		Name:      "source",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "secondary",
	}); !errors.Is(err, domain.ErrRepositoryBlobStoreInUse) {
		t.Fatalf("move nonempty repository error = %v", err)
	}
	persisted, err := metadata.Repository(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.BlobStore != "default" {
		t.Fatalf("failed move changed blob store to %q", persisted.BlobStore)
	}
	if err := metadata.UpdateRepository(ctx, domain.Repository{
		Name:      "source",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "default",
	}); err != nil {
		t.Fatalf("idempotent repository blob store update: %v", err)
	}
}

func TestSQLiteRejectsUnknownRepositoryBlobStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	err := metadata.CreateRepository(ctx, domain.Repository{
		Name:      "missing-store",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "does-not-exist",
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateRepository() error = %v, want not found", err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "existing",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	err = metadata.UpdateRepository(ctx, domain.Repository{
		Name:      "existing",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "does-not-exist",
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpdateRepository() error = %v, want not found", err)
	}
}

func TestSQLiteReferencesAreScopedByBlobStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []domain.Repository{
		{Name: "primary", Format: "raw", Type: "hosted", BlobStore: "default"},
		{Name: "secondary", Format: "raw", Type: "hosted", BlobStore: "secondary"},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "primary",
		Path:       "artifact",
		Digest:     digest,
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}

	primary, err := metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := metadata.ReferencedDigests(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := primary[digest]; !found {
		t.Fatalf("primary references = %v", primary)
	}
	if _, found := secondary[digest]; found {
		t.Fatalf("secondary references unexpectedly include %s", digest)
	}
}

// TestSQLiteReferencesFollowAssetHomeStore proves GC scoping keys on the asset's
// own blob store, not its repository's: an asset relocated to another store is
// referenced there and not in the repository's store.
func TestSQLiteReferencesFollowAssetHomeStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("c", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "primary", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	const digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "primary",
		Path:       "artifact",
		Digest:     digest,
		Size:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh asset inherits its repository's store as its home store.
	if asset.BlobStore != "default" {
		t.Fatalf("new asset home store = %q, want default", asset.BlobStore)
	}

	// Relocate the asset to "secondary" while its repository stays on "default",
	// the divergence the per-asset column exists to allow.
	if _, err := metadata.db.ExecContext(
		ctx,
		`UPDATE assets SET blob_store = ? WHERE repository_id = (SELECT id FROM repositories WHERE name = ?) AND path = ?`,
		"secondary", "primary", "artifact",
	); err != nil {
		t.Fatal(err)
	}

	onDefault, err := metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	onSecondary, err := metadata.ReferencedDigests(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := onSecondary[digest]; !found {
		t.Fatalf("secondary references = %v, want %s", onSecondary, digest)
	}
	if _, found := onDefault[digest]; found {
		t.Fatalf("default still references relocated %s", digest)
	}
}

func TestSQLiteOCIReferencesFollowBlobHomeStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("d", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "images", Format: "oci", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}

	const blobDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "images",
		Path:       "v2/example/image/blobs/" + blobDigest,
		Digest:     blobDigest,
		BlobStore:  "secondary",
		Kind:       "oci-blob",
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := metadata.PutAsset(ctx, domain.Asset{
		Repository:   "images",
		Path:         "v2/example/image/manifests/latest",
		Digest:       "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		BlobStore:    "default",
		Kind:         "oci-manifest",
		Reference:    "latest",
		Dependencies: []string{blobDigest},
		Size:         1,
	})
	if err != nil {
		t.Fatal(err)
	}

	onSecondary, err := metadata.ReferencedDigests(ctx, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := onSecondary[blobDigest]; !found {
		t.Fatalf("secondary references = %v, want dependent blob %s", onSecondary, blobDigest)
	}
	onDefault, err := metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := onDefault[blobDigest]; found {
		t.Fatalf("default references attributed source-store blob to manifest store: %v", onDefault)
	}
	if deleted, err := metadata.DeleteUnreferencedBlobAssets(
		ctx,
		"secondary",
		blobDigest,
	); err != nil || deleted != 0 {
		t.Fatalf("deleted live blob metadata = %d, error = %v", deleted, err)
	}
	if _, err := metadata.DeleteAsset(ctx, "images", manifest.Path); err != nil {
		t.Fatal(err)
	}
	if deleted, err := metadata.DeleteUnreferencedBlobAssets(
		ctx,
		"secondary",
		blobDigest,
	); err != nil || deleted != 1 {
		t.Fatalf("deleted orphan blob metadata = %d, error = %v, want 1", deleted, err)
	}
}

// TestSQLiteStorageUsagePerRepositoryAndStore checks the two dedup semantics:
// a blob shared by two repositories counts toward each repository's footprint,
// but is deduplicated within the blob store that holds it.
func TestSQLiteStorageUsagePerRepositoryAndStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("e", 64),
	}); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []domain.Repository{
		{Name: "a", Format: "raw", Type: "hosted", BlobStore: "default"},
		{Name: "b", Format: "raw", Type: "hosted", BlobStore: "default"},
		{Name: "c", Format: "raw", Type: "hosted", BlobStore: "secondary"},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	shared := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	// "a" and "b" both reference the same 100-byte blob on the default store.
	for _, repo := range []string{"a", "b"} {
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: repo, Path: "artifact", Digest: shared, Size: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "c", Path: "artifact", Digest: other, Size: 50,
	}); err != nil {
		t.Fatal(err)
	}

	usage, err := metadata.StorageUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repoBytes := map[string]int64{}
	for _, row := range usage.Repositories {
		repoBytes[row.Repository] = row.Bytes
	}
	if repoBytes["a"] != 100 || repoBytes["b"] != 100 || repoBytes["c"] != 50 {
		t.Fatalf("repository bytes = %v, want a=100 b=100 c=50", repoBytes)
	}
	storeBytes := map[string]int64{}
	for _, row := range usage.BlobStores {
		storeBytes[row.BlobStore] = row.Bytes
	}
	// Shared blob counted once in the default store; the other on secondary.
	if storeBytes["default"] != 100 || storeBytes["secondary"] != 50 {
		t.Fatalf("blob store bytes = %v, want default=100 secondary=50", storeBytes)
	}
}

// TestSQLiteWriteRedirectsAwayFromDrainingStore proves a draining store takes no
// new assets: WriteBlobStore resolves to the drain target, and an asset written
// to a repository still bound to the draining store lands on the target.
func TestSQLiteWriteRedirectsAwayFromDrainingStore(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	for name, env := range map[string]struct{ envName, fill string }{
		"source": {"SUXEN_TEST_SOURCE", "1"},
		"target": {"SUXEN_TEST_TARGET", "2"},
	} {
		if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
			Name:   name,
			Driver: "memory",
			ConfigurationRef: &domain.ConfigurationReference{
				Env: env.envName,
			},
			PhysicalIdentity: strings.Repeat(env.fill, 64),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The repository binds to source while it is still active.
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "repo", Format: "raw", Type: "hosted", BlobStore: "source",
	}); err != nil {
		t.Fatal(err)
	}

	// Active source: writes stay on source.
	if resolved, err := metadata.WriteBlobStore(ctx, "source"); err != nil || resolved != "source" {
		t.Fatalf("WriteBlobStore(active) = %q, %v; want source", resolved, err)
	}

	// Drain source onto target: writes redirect.
	if err := metadata.SetBlobStoreState(ctx, "source", domain.BlobStoreStateDraining, "target"); err != nil {
		t.Fatal(err)
	}
	if resolved, err := metadata.WriteBlobStore(ctx, "source"); err != nil || resolved != "target" {
		t.Fatalf("WriteBlobStore(draining) = %q, %v; want target", resolved, err)
	}
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "repo",
		Path:       "artifact",
		Digest:     "sha256:" + strings.Repeat("a", 64),
		Size:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.BlobStore != "target" {
		t.Fatalf("asset home store = %q, want target (redirected from draining source)", asset.BlobStore)
	}
}

func TestSQLiteBlobStatsUseStoreAndDigestIdentity(t *testing.T) {
	metadata := openMigrationTestSQLite(t)
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_SECONDARY",
		},
		PhysicalIdentity: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []domain.Repository{
		{Name: "primary", Format: "raw", Type: "hosted", BlobStore: "default"},
		{Name: "secondary", Format: "raw", Type: "hosted", BlobStore: "secondary"},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, repositoryName := range []string{"primary", "secondary"} {
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: repositoryName,
			Path:       "artifact",
			Digest:     digest,
			Size:       7,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := metadata.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UniqueBlobs != 2 || stats.Bytes != 14 {
		t.Fatalf("blob stats = %+v", stats)
	}
}
