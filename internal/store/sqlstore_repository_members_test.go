package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestRepositoryMembersTableDualWrite verifies that group membership is
// dual-written into the repository_members relation alongside the JSON column: a
// group's members are written there, an update rewrites them, deleting a still
// referenced member is rejected (from the authoritative JSON list, which every
// binary maintains), and deleting a group cascades its rows via the group foreign
// key.
func TestRepositoryMembersTableDualWrite(t *testing.T) {
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

			if got := repositoryMembersOf(t, metadata, "g"); !equalStrings(got, []string{"a", "b"}) {
				t.Fatalf("members after create = %v, want [a b]", got)
			}

			// A member still referenced cannot be deleted.
			if err := metadata.DeleteRepository(ctx, "a", Ownership{}); !errors.Is(err, domain.ErrRepositoryInUseByGroup) {
				t.Fatalf("delete of referenced member = %v, want ErrRepositoryInUseByGroup", err)
			}

			// Updating the group drops b from the relation.
			if err := metadata.UpdateRepository(ctx, domain.Repository{
				Name: "g", Format: "raw", Type: "group", Members: []string{"a"},
			}); err != nil {
				t.Fatal(err)
			}
			if got := repositoryMembersOf(t, metadata, "g"); !equalStrings(got, []string{"a"}) {
				t.Fatalf("members after update = %v, want [a]", got)
			}
			if err := metadata.DeleteRepository(ctx, "b", Ownership{}); err != nil {
				t.Fatalf("delete of removed member = %v, want nil", err)
			}

			// Deleting the group cascades its membership rows.
			if err := metadata.DeleteRepository(ctx, "g", Ownership{}); err != nil {
				t.Fatal(err)
			}
			if got := repositoryMembersOf(t, metadata, "g"); len(got) != 0 {
				t.Fatalf("membership rows survived group deletion: %v", got)
			}
			if err := metadata.DeleteRepository(ctx, "a", Ownership{}); err != nil {
				t.Fatalf("delete of no-longer-referenced member = %v, want nil", err)
			}
		})
	}
}

// TestRepositoryMembersDeduplicatesDuplicateList covers a member list that
// legitimately repeats a name — member validation does not reject duplicates, so
// a historical group can hold ["a", "a", "b"]. The relation's (group_name,
// member_name) primary key is a set, so both the dual-write and the migration
// backfill must collapse the repeat to a single row without failing on the
// primary key, and both must record each member at its first-occurrence index so
// a repeat does not shift a later member's position (here a=0, b=2).
func TestRepositoryMembersDeduplicatesDuplicateList(t *testing.T) {
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
			// A group whose member list repeats a name must still dual-write.
			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "g", Format: "raw", Type: "group", Members: []string{"a", "a", "b"},
			}); err != nil {
				t.Fatalf("create group with duplicate members: %v", err)
			}
			wantPositions := map[string]int{"a": 0, "b": 2}
			if got := repositoryMemberPositions(t, metadata, "g"); !equalPositions(got, wantPositions) {
				t.Fatalf("member positions after duplicate create = %v, want %v", got, wantPositions)
			}

			// The migration backfill must produce the identical representation.
			if _, err := metadata.db.ExecContext(ctx, `DELETE FROM repository_members`); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.db.ExecContext(ctx, backfillStatement(t, backend)); err != nil {
				t.Fatalf("replay backfill over duplicate list: %v", err)
			}
			if got := repositoryMemberPositions(t, metadata, "g"); !equalPositions(got, wantPositions) {
				t.Fatalf("member positions after duplicate backfill = %v, want %v", got, wantPositions)
			}
		})
	}
}

// TestRepositoryMembersOnlyForGroups verifies the dual-write matches the backfill's
// WHERE type = 'group' filter: repository validation permits a non-group to carry a
// member list, but that list is not group membership and must not appear in the
// relation, so both origins produce the same (empty) representation.
func TestRepositoryMembersOnlyForGroups(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "a", Format: "raw", Type: "hosted", Writable: true,
			}); err != nil {
				t.Fatal(err)
			}
			// A non-group carrying a member list must not populate the relation.
			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "h", Format: "raw", Type: "hosted", Members: []string{"a"},
			}); err != nil {
				t.Fatalf("create non-group with members: %v", err)
			}
			if got := repositoryMembersOf(t, metadata, "h"); len(got) != 0 {
				t.Fatalf("non-group dual-write populated the relation: %v", got)
			}

			// The backfill reaches the same state from the same rows.
			if _, err := metadata.db.ExecContext(ctx, `DELETE FROM repository_members`); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.db.ExecContext(ctx, backfillStatement(t, backend)); err != nil {
				t.Fatalf("replay backfill: %v", err)
			}
			if got := repositoryMembersOf(t, metadata, "h"); len(got) != 0 {
				t.Fatalf("backfill populated the relation for a non-group: %v", got)
			}
		})
	}
}

func repositoryMembersOf(t *testing.T, metadata *SQLStore, group string) []string {
	t.Helper()
	rows, err := metadata.db.QueryContext(context.Background(),
		`SELECT member_name FROM repository_members WHERE group_name = ? ORDER BY position`, group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		members = append(members, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return members
}

func repositoryMemberPositions(t *testing.T, metadata *SQLStore, group string) map[string]int {
	t.Helper()
	rows, err := metadata.db.QueryContext(context.Background(),
		`SELECT member_name, position FROM repository_members WHERE group_name = ?`, group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	positions := make(map[string]int)
	for rows.Next() {
		var name string
		var position int
		if err := rows.Scan(&name, &position); err != nil {
			t.Fatal(err)
		}
		positions[name] = position
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return positions
}

func equalPositions(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for name, position := range want {
		if got[name] != position {
			return false
		}
	}
	return true
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
