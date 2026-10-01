package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestLocalAuthorizationRejectsReplacementAccount(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		const username = "account-identity-user"
		if err := metadata.CreateRole(ctx, domain.Role{Name: "identity-writer", Privileges: []string{"repository:identity:write"}}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateUser(ctx, username, "first-password", false); err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetUserRoles(ctx, username, []string{"identity-writer"}); err != nil {
			t.Fatal(err)
		}
		original, ok := metadata.AuthenticatePassword(ctx, username, "first-password")
		if !ok || original.Identity == "" {
			t.Fatalf("first password authentication = %+v, %v", original, ok)
		}
		current, privileges, err := metadata.LocalAuthorization(ctx, username, original.Identity)
		if err != nil || current.Admin || len(current.Roles) != 1 || len(privileges) == 0 {
			t.Fatalf("first authorization = %+v, %v, %v", current, privileges, err)
		}
		if err := metadata.DeleteUser(ctx, username, Ownership{}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateUser(ctx, username, "second-password", true); err != nil {
			t.Fatal(err)
		}
		replacement, ok := metadata.AuthenticatePassword(ctx, username, "second-password")
		if !ok || replacement.Identity == "" || replacement.Identity == original.Identity {
			t.Fatalf("replacement identity = %+v, authenticated=%v", replacement, ok)
		}
		if _, _, err := metadata.LocalAuthorization(ctx, username, original.Identity); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("old identity authorization error = %v, want not found", err)
		}
		current, _, err = metadata.LocalAuthorization(ctx, username, replacement.Identity)
		if err != nil || !current.Admin {
			t.Fatalf("replacement authorization = %+v, %v", current, err)
		}
	})
}

func TestAccountIdentityMigrationBackfillsDistinctStableValues(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		const username = "migration-account-one"
		const apiSecret = "migration-api-secret"
		if err := metadata.CreateRole(ctx, domain.Role{Name: "migration-role", Privileges: []string{"repository:migration:read"}}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateUser(ctx, username, "migration-password", true); err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetUserRoles(ctx, username, []string{"migration-role"}); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.CreateToken(ctx, username, "migration-token", apiSecret, []string{"repository:migration:read"}); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 14`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `DROP INDEX idx_users_identity`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `ALTER TABLE users DROP COLUMN identity`); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `ALTER TABLE cleanup_policies DROP COLUMN retention_order`); err != nil {
			t.Fatal(err)
		}
		dropAssetComponentSchema(t, metadata)
		if _, err := metadata.db.ExecContext(ctx,
			`INSERT INTO users (username, password_hash, admin, created_at) VALUES (?, ?, ?, ?)`,
			"migration-account-two", "legacy-hash", false, "2026-09-28T00:00:00.000000000Z"); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		one, err := metadata.User(ctx, "migration-account-one")
		if err != nil {
			t.Fatal(err)
		}
		two, err := metadata.User(ctx, "migration-account-two")
		if err != nil {
			t.Fatal(err)
		}
		if one.Identity == "" || two.Identity == "" || one.Identity == two.Identity {
			t.Fatalf("backfilled identities = %q, %q", one.Identity, two.Identity)
		}
		passwordUser, ok := metadata.AuthenticatePassword(ctx, username, "migration-password")
		if !ok || !passwordUser.Admin || passwordUser.Identity != one.Identity {
			t.Fatalf("migrated password authentication = %+v, %v", passwordUser, ok)
		}
		tokenUser, ok := metadata.AuthenticateToken(ctx, apiSecret)
		if !ok || !tokenUser.Admin || tokenUser.Identity != one.Identity || len(tokenUser.TokenScopes) != 1 {
			t.Fatalf("migrated API token authentication = %+v, %v", tokenUser, ok)
		}
		authorized, privileges, err := metadata.LocalAuthorization(ctx, username, one.Identity)
		if err != nil || !authorized.Admin || len(authorized.Roles) != 1 || len(privileges) == 0 {
			t.Fatalf("migrated authorization = %+v, %v, %v", authorized, privileges, err)
		}
		if err := metadata.UpdateUser(ctx, username, "new-migration-password", false); err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetUserRoles(ctx, username, nil); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		stable, err := metadata.User(ctx, username)
		if err != nil || stable.Identity != one.Identity {
			t.Fatalf("identity after repeated migrate = %q, %v", stable.Identity, err)
		}
	})
}
