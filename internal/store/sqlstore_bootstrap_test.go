package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestBootstrapAdminRollsBackEveryCredentialOnLaterFailure(t *testing.T) {
	for _, table := range []string{"user_roles", "tokens"} {
		t.Run(table, func(t *testing.T) {
			metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer metadata.Close()
			ctx := context.Background()
			migrateTestMetadata(t, metadata)
			if err := metadata.CreateRole(ctx, domain.Role{Name: "administrator"}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.db.ExecContext(ctx,
				`CREATE TRIGGER fail_bootstrap BEFORE INSERT ON `+table+` BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := metadata.CreateBootstrapAdmin(ctx, "admin", "generated-password", "generated-token"); err == nil {
				t.Fatal("expected injected failure")
			}
			if _, err := metadata.User(ctx, "admin"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("partial account committed: %v", err)
			}
			if _, err := metadata.db.ExecContext(ctx, `DROP TRIGGER fail_bootstrap`); err != nil {
				t.Fatal(err)
			}
			if err := metadata.CreateBootstrapAdmin(ctx, "admin", "retry-password", "retry-token"); err != nil {
				t.Fatal(err)
			}
			if _, ok := metadata.AuthenticatePassword(ctx, "admin", "retry-password"); !ok {
				t.Fatal("retry password unusable")
			}
			if _, ok := metadata.AuthenticateToken(ctx, "retry-token"); !ok {
				t.Fatal("retry token unusable")
			}
			roles, err := metadata.UserRoles(ctx, "admin")
			if err != nil || len(roles) != 1 || roles[0] != "administrator" {
				t.Fatalf("retry roles = %v, %v", roles, err)
			}
		})
	}
}

func TestBlobStoreCannotRetireWhileUploadSessionNeedsIt(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name: "secondary", Driver: "fs", PhysicalIdentity: strings.Repeat("a", 64),
		ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_TEST_STORE"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := createUploadTestRepository(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	session := testUploadSession("unfinished", "admin", time.Now().UTC())
	repository, err := metadata.Repository(ctx, "registry")
	if err != nil {
		t.Fatal(err)
	}
	session.RepositoryID = repository.ID
	session.BlobStore = "secondary"
	limits := UploadSessionLimits{MaxStagedBytes: 1024, MaxPrincipalStagedBytes: 1024, MaxPrincipalSessions: 4}
	if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteBlobStore(ctx, "secondary", Ownership{}); !errors.Is(err, domain.ErrBlobStoreInUse) {
		t.Fatalf("retire store with staged upload = %v", err)
	}
	if err := metadata.DeleteUploadSession(ctx, session.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteBlobStore(ctx, "secondary", Ownership{}); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAdminConflictDoesNotRestoreRevokedToken(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRole(ctx, domain.Role{Name: "administrator"}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBootstrapAdmin(ctx, "admin", "original-password", "original-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `DELETE FROM tokens WHERE username = ?`, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBootstrapAdmin(ctx, "admin", "replacement-password", "replacement-token"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("existing account = %v, want conflict", err)
	}
	if _, ok := metadata.AuthenticatePassword(ctx, "admin", "original-password"); !ok {
		t.Fatal("original password changed")
	}
	if _, ok := metadata.AuthenticatePassword(ctx, "admin", "replacement-password"); ok {
		t.Fatal("conflicting bootstrap reset password")
	}
	if _, ok := metadata.AuthenticateToken(ctx, "replacement-token"); ok {
		t.Fatal("conflicting bootstrap restored a revoked token")
	}
}
