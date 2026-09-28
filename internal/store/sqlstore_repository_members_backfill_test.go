package store

import (
	"context"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestRepositoryMembersBackfill exercises the dialect-specific backfill statement
// in migration 0008 against real data: a store that predates the relation has its
// group membership only in the repositories.members JSON column, and the backfill
// must reconstruct the repository_members rows from it. The membership is seeded
// through the store, the relation is emptied to simulate the pre-migration state,
// and the migration's own backfill statement (read from the embedded file so the
// test cannot drift from it) is replayed.
func TestRepositoryMembersBackfill(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			for _, name := range []string{"a", "b"} {
				if err := metadata.CreateRepository(ctx, domain.Repository{
					Name: name, Format: "raw", Type: "hosted", Writable: true,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "g", Format: "raw", Type: "group", Members: []string{"a", "b"},
			}); err != nil {
				t.Fatal(err)
			}

			// Simulate the pre-0008 state: membership lives only in the JSON column.
			if _, err := metadata.db.ExecContext(ctx, `DELETE FROM repository_members`); err != nil {
				t.Fatal(err)
			}
			if got := repositoryMembersOf(t, metadata, "g"); len(got) != 0 {
				t.Fatalf("relation not cleared: %v", got)
			}

			if _, err := metadata.db.ExecContext(ctx, backfillStatement(t, backend)); err != nil {
				t.Fatalf("replay backfill: %v", err)
			}
			if got := repositoryMembersOf(t, metadata, "g"); !equalStrings(got, []string{"a", "b"}) {
				t.Fatalf("members after backfill = %v, want [a b]", got)
			}
		})
	}
}

// backfillStatement reads migration 0008's INSERT from the embedded file so the
// test replays exactly what the migration runs, not a copy that could drift.
func backfillStatement(t *testing.T, dialect string) string {
	t.Helper()
	content, err := migrationFiles.ReadFile("migrations/" + dialect + "/0008_repository_members.sql")
	if err != nil {
		t.Fatal(err)
	}
	marker := "INSERT INTO repository_members"
	index := strings.Index(string(content), marker)
	if index < 0 {
		t.Fatalf("backfill INSERT not found in 0008 migration for %s", dialect)
	}
	return string(content)[index:]
}
