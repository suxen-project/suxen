package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

const (
	dependencyPendingDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dependencyManifestOne   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	dependencyManifestTwo   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// seedAssetDependencyStores creates a secondary blob store and an OCI repository
// so a test can publish blobs and manifests across two stores.
func seedAssetDependencyStores(t *testing.T, metadata *SQLStore) {
	t.Helper()
	ctx := context.Background()
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "secondary",
		Driver: "memory",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_TEST_DEP_SECONDARY",
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
}

func putManifestWithDependency(t *testing.T, metadata *SQLStore, reference, manifestDigest, dependency string) domain.Asset {
	t.Helper()
	manifest, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository:   "images",
		Path:         "v2/example/image/manifests/" + reference,
		Digest:       manifestDigest,
		BlobStore:    "default",
		Kind:         "oci-manifest",
		Reference:    reference,
		Dependencies: []string{dependency},
		Size:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func putBlob(t *testing.T, metadata *SQLStore, digest, blobStore string) {
	t.Helper()
	if _, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "images",
		Path:       "v2/example/image/blobs/" + digest,
		Digest:     digest,
		BlobStore:  blobStore,
		Kind:       "oci-blob",
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}
}

// dependencyStore returns the resolved home store recorded on a dependency edge,
// and whether it is resolved (non-NULL). A NULL blob_store is UNRESOLVED.
func dependencyStore(t *testing.T, metadata *SQLStore, digest string) (string, bool) {
	t.Helper()
	var store sql.NullString
	if err := metadata.db.QueryRowContext(context.Background(),
		`SELECT blob_store FROM asset_dependencies WHERE digest = ?`, digest,
	).Scan(&store); err != nil {
		t.Fatal(err)
	}
	return store.String, store.Valid
}

// TestAssetDependencyUnresolvedPinsEveryStore covers the gap the store column
// closes: a manifest whose depended-on blob is not published yet (a proxy pull
// where the manifest is cached before its layers) records an UNRESOLVED edge, and
// that edge must pin its digest as live in every store so garbage collection
// never reclaims the blob's bytes once they appear.
func TestAssetDependencyUnresolvedPinsEveryStore(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			seedAssetDependencyStores(t, metadata)

			putManifestWithDependency(t, metadata, "latest", dependencyManifestOne, dependencyPendingDigest)

			// The blob is absent, so the edge is UNRESOLVED.
			if store, resolved := dependencyStore(t, metadata, dependencyPendingDigest); resolved {
				t.Fatalf("dependency edge resolved to %q with no published blob", store)
			}

			for _, name := range []string{"default", "secondary"} {
				referenced, err := metadata.ReferencedDigests(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				if _, pinned := referenced[dependencyPendingDigest]; !pinned {
					t.Fatalf("store %q references = %v, want unresolved dependency %s pinned",
						name, referenced, dependencyPendingDigest)
				}
			}
		})
	}
}

// TestAssetDependencyResolvesOnBlobPublish covers the resolve-under-lease
// transition: an UNRESOLVED edge is narrowed to the blob's actual home store when
// the blob is published, so its digest stops pinning every store and is pinned
// only where the blob lives.
func TestAssetDependencyResolvesOnBlobPublish(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			seedAssetDependencyStores(t, metadata)

			putManifestWithDependency(t, metadata, "latest", dependencyManifestOne, dependencyPendingDigest)
			if _, resolved := dependencyStore(t, metadata, dependencyPendingDigest); resolved {
				t.Fatal("edge resolved before the blob was published")
			}

			// Publishing the blob into secondary resolves the edge to it.
			putBlob(t, metadata, dependencyPendingDigest, "secondary")
			if store, resolved := dependencyStore(t, metadata, dependencyPendingDigest); !resolved || store != "secondary" {
				t.Fatalf("edge after publish = (%q, resolved=%v), want secondary", store, resolved)
			}

			// The pin has narrowed: secondary still pins (its blob row is there),
			// default no longer does (the edge is resolved and the blob is not in
			// default).
			onSecondary, err := metadata.ReferencedDigests(ctx, "secondary")
			if err != nil {
				t.Fatal(err)
			}
			if _, pinned := onSecondary[dependencyPendingDigest]; !pinned {
				t.Fatalf("secondary references = %v, want resolved dependency pinned", onSecondary)
			}
			onDefault, err := metadata.ReferencedDigests(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			if _, pinned := onDefault[dependencyPendingDigest]; pinned {
				t.Fatalf("default still pins a dependency resolved to secondary: %v", onDefault)
			}
		})
	}
}

// TestAssetDependencyResolvesAtWriteWhenBlobPresent covers the direct-push order:
// when a manifest is published after its blob, the edge resolves to the blob's
// home store immediately and is never UNRESOLVED, so it does not spuriously pin
// the digest in the manifest's own store.
func TestAssetDependencyResolvesAtWriteWhenBlobPresent(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			seedAssetDependencyStores(t, metadata)

			putBlob(t, metadata, dependencyPendingDigest, "secondary")
			putManifestWithDependency(t, metadata, "latest", dependencyManifestOne, dependencyPendingDigest)

			if store, resolved := dependencyStore(t, metadata, dependencyPendingDigest); !resolved || store != "secondary" {
				t.Fatalf("edge = (%q, resolved=%v), want secondary resolved at write", store, resolved)
			}
			onDefault, err := metadata.ReferencedDigests(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			if _, pinned := onDefault[dependencyPendingDigest]; pinned {
				t.Fatalf("manifest store default pins a blob homed in secondary: %v", onDefault)
			}
		})
	}
}

// TestAssetDependencyNullEdgeDoesNotOverpinPublishedBlob covers the convergence
// requirement: an edge can remain UNRESOLVED (NULL) even after its blob is
// published — an old binary that inserts an edge without resolving, or a
// publication-order race where the resolver misses the edge. Such a NULL edge
// must not pin its digest in stores that do not hold the blob; the join arm
// already pins the store that does. Otherwise a stale NULL would over-retain an
// orphaned copy of the digest elsewhere forever.
func TestAssetDependencyNullEdgeDoesNotOverpinPublishedBlob(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			seedAssetDependencyStores(t, metadata)

			// The blob is published in secondary and a manifest depends on it.
			putBlob(t, metadata, dependencyPendingDigest, "secondary")
			putManifestWithDependency(t, metadata, "latest", dependencyManifestOne, dependencyPendingDigest)

			// Simulate an unresolved edge that persists despite the published blob
			// (an old writer, or a lost publication-order race).
			if _, err := metadata.db.ExecContext(ctx,
				`UPDATE asset_dependencies SET blob_store = NULL WHERE digest = ?`,
				dependencyPendingDigest); err != nil {
				t.Fatal(err)
			}
			if store, resolved := dependencyStore(t, metadata, dependencyPendingDigest); resolved {
				t.Fatalf("edge unexpectedly resolved to %q after forcing NULL", store)
			}

			// secondary holds the blob and is pinned via the join arm; default does
			// not hold it and must not be pinned by the stale NULL edge.
			onSecondary, err := metadata.ReferencedDigests(ctx, "secondary")
			if err != nil {
				t.Fatal(err)
			}
			if _, pinned := onSecondary[dependencyPendingDigest]; !pinned {
				t.Fatalf("secondary references = %v, want the published blob pinned", onSecondary)
			}
			onDefault, err := metadata.ReferencedDigests(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			if _, pinned := onDefault[dependencyPendingDigest]; pinned {
				t.Fatalf("stale NULL edge over-pinned the digest in default: %v", onDefault)
			}
		})
	}
}

// TestAssetDependencyBackfill replays migration 0009's own backfill statement (read
// from the embedded file so the test cannot drift from it) against a pre-migration
// state where edges carry no store: one whose blob is published must resolve to it,
// one whose blob is absent must stay UNRESOLVED.
func TestAssetDependencyBackfill(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			seedAssetDependencyStores(t, metadata)

			// A published blob and a manifest that depends on it, plus a second
			// manifest depending on a blob that is absent.
			putBlob(t, metadata, dependencyPendingDigest, "secondary")
			putManifestWithDependency(t, metadata, "present", dependencyManifestOne, dependencyPendingDigest)
			const absentDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			putManifestWithDependency(t, metadata, "absent", dependencyManifestTwo, absentDigest)

			// Simulate the pre-0009 state: no edge carries a store.
			if _, err := metadata.db.ExecContext(ctx, `UPDATE asset_dependencies SET blob_store = NULL`); err != nil {
				t.Fatal(err)
			}

			if _, err := metadata.db.ExecContext(ctx, assetDependencyBackfillStatement(t, backend)); err != nil {
				t.Fatalf("replay backfill: %v", err)
			}

			if store, resolved := dependencyStore(t, metadata, dependencyPendingDigest); !resolved || store != "secondary" {
				t.Fatalf("published-blob edge after backfill = (%q, resolved=%v), want secondary", store, resolved)
			}
			if store, resolved := dependencyStore(t, metadata, absentDigest); resolved {
				t.Fatalf("absent-blob edge resolved to %q, want UNRESOLVED", store)
			}
		})
	}
}

// assetDependencyBackfillStatement extracts migration 0009's UPDATE from the
// embedded file so the test replays exactly what the migration runs.
func assetDependencyBackfillStatement(t *testing.T, dialect string) string {
	t.Helper()
	content, err := migrationFiles.ReadFile("migrations/" + dialect + "/0009_asset_dependency_blob_store.sql")
	if err != nil {
		t.Fatal(err)
	}
	marker := "UPDATE asset_dependencies"
	index := strings.Index(string(content), marker)
	if index < 0 {
		t.Fatalf("backfill UPDATE not found in 0009 migration for %s", dialect)
	}
	return string(content)[index:]
}
