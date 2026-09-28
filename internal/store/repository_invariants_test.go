package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestUpdateRepositoryRejectsTypeAndFormatChange(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	base := domain.Repository{Name: "repo", Format: "raw", Type: "hosted"}
	if err := metadata.CreateRepository(ctx, base); err != nil {
		t.Fatal(err)
	}

	// An update that leaves type and format untouched is valid.
	writable := base
	writable.Writable = true
	if err := metadata.UpdateRepository(ctx, writable); err != nil {
		t.Fatalf("unchanged type/format update rejected: %v", err)
	}

	formatChange := base
	formatChange.Format = "oci"
	if err := metadata.UpdateRepository(ctx, formatChange); !errors.Is(err, domain.ErrImmutableRepositoryField) {
		t.Fatalf("format change error = %v, want immutable repository field", err)
	}

	typeChange := base
	typeChange.Type = "proxy"
	typeChange.Upstream = "https://example.test/raw"
	if err := metadata.UpdateRepository(ctx, typeChange); !errors.Is(err, domain.ErrImmutableRepositoryField) {
		t.Fatalf("type change error = %v, want immutable repository field", err)
	}

	stored, err := metadata.Repository(ctx, base.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Type != "hosted" || stored.Format != "raw" {
		t.Fatalf("rejected changes persisted: %+v", stored)
	}
}

func TestUpdateRepositoryRejectsUpstreamEndpointChange(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	base := domain.Repository{
		Name:     "proxy",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://origin.example.test/raw",
	}
	if err := metadata.CreateRepository(ctx, base); err != nil {
		t.Fatal(err)
	}

	// Rotating only the embedded credentials keeps the same endpoint: allowed.
	rotated := base
	rotated.Upstream = "https://user:secret@origin.example.test/raw"
	if err := metadata.UpdateRepository(ctx, rotated); err != nil {
		t.Fatalf("credential rotation rejected: %v", err)
	}

	// Changing the endpoint (host or path) is rejected.
	for _, changed := range []string{
		"https://origin.example.test/other",
		"https://elsewhere.example.test/raw",
	} {
		endpoint := base
		endpoint.Upstream = changed
		if err := metadata.UpdateRepository(ctx, endpoint); !errors.Is(
			err, domain.ErrImmutableRepositoryField,
		) {
			t.Fatalf("endpoint change to %q = %v, want immutable repository field", changed, err)
		}
	}
}

func TestRepositoryRecreateGetsFreshIdentityAndEmptyState(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	repository := domain.Repository{Name: "reused", Format: "raw", Type: "hosted", Writable: true}
	if err := metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	firstID := repositoryIDForTest(t, metadata, "reused")

	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "reused",
		Path:       "artifact",
		Digest:     "sha256:" + strings.Repeat("a", 64),
		Size:       1,
		BlobStore:  "default",
	}); err != nil {
		t.Fatal(err)
	}

	if err := metadata.DeleteRepository(ctx, "reused", Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	secondID := repositoryIDForTest(t, metadata, "reused")
	if secondID == firstID {
		t.Fatalf("recreated repository reused the internal id %q", secondID)
	}

	// The cascade deletion left the replacement with no inherited assets.
	assets, err := metadata.Assets(ctx, "reused", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("recreated repository inherited %d assets, want 0", len(assets))
	}
}

func TestUpdateRepositoryPreservesInternalID(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	repo := domain.Repository{Name: "keep", Format: "raw", Type: "hosted"}
	if err := metadata.CreateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	before, err := metadata.Repository(ctx, "keep")
	if err != nil {
		t.Fatal(err)
	}
	repo.Writable = true
	if err := metadata.UpdateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	after, err := metadata.Repository(ctx, "keep")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatalf("ordinary update changed the internal id %q -> %q", before.ID, after.ID)
	}
}

func TestPinnedWritesRejectedAfterSameNameRecreate(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	repo := domain.Repository{Name: "proxy", Format: "raw", Type: "hosted", Writable: true}
	if err := metadata.CreateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	first, err := metadata.Repository(ctx, "proxy")
	if err != nil {
		t.Fatal(err)
	}

	// Delete and recreate under the same public name: a fresh internal identity.
	if err := metadata.DeleteRepository(ctx, "proxy", Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	second, err := metadata.Repository(ctx, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("recreate reused the internal id %q", second.ID)
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	// A write pinned to the OLD identity (an in-flight fetch that started before
	// the recreate) must be rejected, never redirected into the replacement.
	stale := domain.Asset{
		Repository:   "proxy",
		RepositoryID: first.ID,
		Path:         "artifact",
		Digest:       digest,
		Size:         1,
		BlobStore:    "default",
	}
	if _, err := metadata.PutAsset(ctx, stale); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale pinned asset write = %v, want not found", err)
	}
	if err := metadata.PutNegativeCacheByRepositoryID(ctx, first.ID, "artifact", time.Now().Add(time.Minute)); !errors.Is(
		err, domain.ErrNotFound,
	) {
		t.Fatalf("stale pinned negative-cache write = %v, want not found", err)
	}

	// The replacement is untouched: no assets, no inherited negative cache.
	assets, err := metadata.Assets(ctx, "proxy", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("replacement inherited %d assets", len(assets))
	}
	if hit, err := metadata.NegativeCacheHit(ctx, "proxy", "artifact", time.Now()); err != nil || hit {
		t.Fatalf("replacement inherited negative cache: hit=%v err=%v", hit, err)
	}

	// A write pinned to the CURRENT identity succeeds.
	fresh := stale
	fresh.RepositoryID = second.ID
	if _, err := metadata.PutAsset(ctx, fresh); err != nil {
		t.Fatalf("current pinned asset write rejected: %v", err)
	}
}

func TestImmutableTypeFormatTriggerRejectsRawUpdate(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "guarded", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	// A direct UPDATE bypassing the store's code check must still be aborted by
	// the schema trigger (defense in depth).
	if _, err := metadata.db.ExecContext(
		ctx,
		`UPDATE repositories SET format = ? WHERE name = ?`,
		"oci", "guarded",
	); err == nil {
		t.Fatal("raw format change succeeded, want trigger abort")
	}
}

func repositoryIDForTest(t *testing.T, metadata *SQLStore, name string) string {
	t.Helper()
	var id string
	if err := metadata.db.QueryRowContext(
		context.Background(),
		`SELECT id FROM repositories WHERE name = ?`,
		name,
	).Scan(&id); err != nil {
		t.Fatalf("read repository id: %v", err)
	}
	return id
}

func TestDeleteRepositoryRejectedWhenReferencedByGroup(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "leaf", Format: "raw", Type: "hosted"},
		{Name: "grp", Format: "raw", Type: "group", Members: []string{"leaf"}},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}

	if err := metadata.DeleteRepository(ctx, "leaf", Ownership{}); !errors.Is(err, domain.ErrRepositoryInUseByGroup) {
		t.Fatalf("delete of referenced member error = %v, want repository in use by group", err)
	}
	if _, err := metadata.Repository(ctx, "leaf"); err != nil {
		t.Fatalf("rejected delete removed the member: %v", err)
	}

	// Once the group no longer references it, the member deletes.
	if err := metadata.DeleteRepository(ctx, "grp", Ownership{}); err != nil {
		t.Fatalf("group delete failed: %v", err)
	}
	if err := metadata.DeleteRepository(ctx, "leaf", Ownership{}); err != nil {
		t.Fatalf("member delete after group removal failed: %v", err)
	}
}
