package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestUserAndRoleWritesRollbackTogether(t *testing.T) {
	ctx := context.Background()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	migrateTestMetadata(t, metadata)

	if err := metadata.SaveUser(ctx, UserSave{
		Username: "new-user", Password: "new-password", Admin: true,
		Roles: []string{"missing"}, Create: true,
	}); err == nil {
		t.Fatal("creating with a missing role succeeded")
	}
	if _, err := metadata.User(ctx, "new-user"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed create left user behind: %v", err)
	}

	if err := metadata.CreateRole(ctx, domain.Role{Name: "retained", Privileges: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SaveUser(ctx, UserSave{
		Username: "existing", Password: "original-password", Admin: false,
		Roles: []string{"retained"}, Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PutProvisionRecord(ctx, ProvisionRecord{
		Kind: "user", Name: "existing", SecretFingerprint: "original-fingerprint",
	}); err != nil {
		t.Fatal(err)
	}
	// A declarative update whose role is missing must roll the account, its
	// assignments, and its ownership fingerprint back together.
	if err := metadata.SaveUser(ctx, UserSave{
		Username: "existing", Password: "replacement-password", Admin: true,
		Roles: []string{"missing"}, Create: false,
		Ownership: Ownership{Declarative: true, Fingerprint: "replacement-fingerprint"},
	}); err == nil {
		t.Fatal("updating with a missing role succeeded")
	}
	user, err := metadata.User(ctx, "existing")
	if err != nil || user.Admin {
		t.Fatalf("failed update changed administrator status: user=%+v err=%v", user, err)
	}
	if _, authenticated := metadata.AuthenticatePassword(ctx, "existing", "original-password"); !authenticated {
		t.Fatal("failed update changed password")
	}
	if _, authenticated := metadata.AuthenticatePassword(ctx, "existing", "replacement-password"); authenticated {
		t.Fatal("failed update accepted replacement password")
	}
	roles, err := metadata.UserRoles(ctx, "existing")
	if err != nil || len(roles) != 1 || roles[0] != "retained" {
		t.Fatalf("failed update changed roles: roles=%v err=%v", roles, err)
	}
	record, err := metadata.ProvisionRecord(ctx, "user", "existing")
	if err != nil || record.SecretFingerprint != "original-fingerprint" {
		t.Fatalf("failed update changed fingerprint: record=%+v err=%v", record, err)
	}
}
