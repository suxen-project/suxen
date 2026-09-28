package store

import (
	"context"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// Roles are flat: privilege resolution is the union of the directly assigned
// roles' privileges (plus anonymous), with no role-to-role expansion. Exercise
// both the local-user and external-identity paths on both dialects, including a
// multi-element IN list.
func TestFlatRolePrivilegeResolutionOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		if err := metadata.CreateUser(ctx, "bot", "a-secure-password", false); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateRole(ctx, domain.Role{Name: "reader", Privileges: []string{"repository:raw:read"}}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateRole(ctx, domain.Role{Name: "writer", Privileges: []string{"repository:raw:write"}}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetUserRoles(ctx, "bot", []string{"reader", "writer"}); err != nil {
			t.Fatal(err)
		}

		local, err := metadata.EffectivePrivileges(ctx, "bot")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(local, ","); !strings.Contains(got, "repository:raw:read") ||
			!strings.Contains(got, "repository:raw:write") {
			t.Fatalf("local privileges = %v", local)
		}

		external, err := metadata.PrivilegesForRoles(ctx, []string{"reader", "writer"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(external, ","); !strings.Contains(got, "repository:raw:read") ||
			!strings.Contains(got, "repository:raw:write") {
			t.Fatalf("external privileges = %v", external)
		}

		single, err := metadata.PrivilegesForRoles(ctx, []string{"reader"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(single, ","); !strings.Contains(got, "repository:raw:read") ||
			strings.Contains(got, "repository:raw:write") {
			t.Fatalf("single-role privileges leaked writer: %v", single)
		}
	})
}
