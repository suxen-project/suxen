package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestRoleOwnershipOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			// A declarative create adopts an ownership record.
			if err := metadata.SaveRole(ctx, RoleSave{
				Role:      domain.Role{Name: "reader", Privileges: []string{"repository:raw:read"}},
				Create:    true,
				Ownership: Ownership{Declarative: true, Fingerprint: "fp-1"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "role", "reader"); err != nil || record.SecretFingerprint != "fp-1" {
				t.Fatalf("declarative create did not adopt record: %+v %v", record, err)
			}

			// An imperative update of a managed role without force is rejected and
			// leaves the privileges untouched.
			if err := metadata.SaveRole(ctx, RoleSave{
				Role:   domain.Role{Name: "reader", Privileges: []string{"repository:raw:write"}},
				Create: false,
			}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative update of managed role = %v, want ErrManaged", err)
			}
			if role, err := metadata.Role(ctx, "reader"); err != nil || len(role.Privileges) != 1 || role.Privileges[0] != "repository:raw:read" {
				t.Fatalf("rejected update still changed privileges: %+v %v", role, err)
			}

			// An imperative update with force transfers ownership: the record is dropped.
			if err := metadata.SaveRole(ctx, RoleSave{
				Role:      domain.Role{Name: "reader", Privileges: []string{"repository:raw:write"}},
				Create:    false,
				Ownership: Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "role", "reader"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("force update did not release ownership: %v", err)
			}

			// An imperative delete of an unmanaged role succeeds.
			if err := metadata.DeleteRole(ctx, "reader", Ownership{}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.Role(ctx, "reader"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("delete left the role: %v", err)
			}
		})
	}
}

// TestSQLiteDeleteRoleRejectedWhenReferencedByProvider exercises the role
// deletion rule: a role still named by an OIDC provider's default or group role
// mapping cannot be deleted, while direct user assignments cascade with it.
func TestSQLiteDeleteRoleRejectedWhenReferencedByProvider(t *testing.T) {
	metadata := openCompanionTestStore(t, "sqlite")
	ctx := context.Background()

	if err := metadata.SaveRole(ctx, RoleSave{
		Role:   domain.Role{Name: "reader", Privileges: []string{"repository:raw:read"}},
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	// An OIDC provider maps a group onto the role.
	if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
		Provider: domain.OIDCProvider{
			Name:       "corp",
			Issuer:     "https://issuer.example.com",
			ClientID:   "client",
			GroupRoles: map[string][]string{"engineers": {"reader"}},
		},
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := metadata.DeleteRole(ctx, "reader", Ownership{}); !errors.Is(err, domain.ErrRoleReferencedByProvider) {
		t.Fatalf("delete of referenced role = %v, want ErrRoleReferencedByProvider", err)
	}
	if _, err := metadata.Role(ctx, "reader"); err != nil {
		t.Fatalf("rejected delete removed the role: %v", err)
	}

	// Removing the mapping frees the role for deletion.
	if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
		Provider: domain.OIDCProvider{
			Name:     "corp",
			Issuer:   "https://issuer.example.com",
			ClientID: "client",
		},
		Create: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.DeleteRole(ctx, "reader", Ownership{}); err != nil {
		t.Fatalf("delete of unreferenced role failed: %v", err)
	}
}

// TestSQLiteSaveOIDCProviderRejectsMappingToMissingRole covers the save side of
// the role-deletion invariant: SaveOIDCProvider verifies each mapped role exists
// inside its transaction, under the role's ownership key. Together with
// DeleteRole rejecting a referenced role under the same key, a provider mapping
// and a role deletion serialize on the role key rather than both committing and
// stranding a mapping to a deleted role. The concurrent interleaving is a
// PostgreSQL advisory-lock property; SQLite serializes writers already, so this
// asserts the in-transaction existence guard deterministically.
func TestSQLiteSaveOIDCProviderRejectsMappingToMissingRole(t *testing.T) {
	metadata := openCompanionTestStore(t, "sqlite")
	ctx := context.Background()

	err := metadata.SaveOIDCProvider(ctx, OIDCSave{
		Provider: domain.OIDCProvider{
			Name:         "corp",
			Issuer:       "https://issuer.example.com",
			ClientID:     "client",
			DefaultRoles: []string{"ghost"},
		},
		Create: true,
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("provider save mapping a missing role = %v, want ErrNotFound", err)
	}

	// The provider must not have been created.
	if _, err := metadata.OIDCProvider(ctx, "corp"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rejected provider save left a provider: %v", err)
	}

	// With the role present, the same save succeeds.
	if err := metadata.SaveRole(ctx, RoleSave{
		Role:   domain.Role{Name: "ghost", Privileges: []string{"repository:raw:read"}},
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
		Provider: domain.OIDCProvider{
			Name:         "corp",
			Issuer:       "https://issuer.example.com",
			ClientID:     "client",
			DefaultRoles: []string{"ghost"},
		},
		Create: true,
	}); err != nil {
		t.Fatalf("provider save mapping an existing role failed: %v", err)
	}
}

// TestPostgresProviderSaveAndRoleDeleteSerialize proves the cross-replica HA
// invariant of the atomic role commands: a provider save and a role deletion
// serialize on the role's ownership advisory key, and whichever one waits on
// the lock re-reads the guarded state after the wait — so the two orders cannot
// both commit and strand an OIDC mapping onto a deleted role. SQLite serializes
// writers implicitly; this is a PostgreSQL advisory-lock guarantee, so the test
// runs only against PostgreSQL (openCompanionTestStore skips otherwise). It is
// deterministic rather than timing-dependent: a controlling transaction holds
// the role key while the operation under test blocks on it (confirmed through
// pg_locks) and commits the competing state before releasing the lock.
func TestPostgresProviderSaveAndRoleDeleteSerialize(t *testing.T) {
	t.Run("provider save commits first, delete rejects", func(t *testing.T) {
		metadata := openCompanionTestStore(t, "postgres")
		ctx := context.Background()
		mustCreateRole(t, metadata, "reader")

		controller, err := metadata.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Rollback()
		if err := lockOwnershipTx(ctx, controller, "role", "reader"); err != nil {
			t.Fatal(err)
		}

		result := make(chan error, 1)
		go func() { result <- metadata.DeleteRole(ctx, "reader", Ownership{}) }()
		waitForRoleKeyWaiter(t, metadata, "reader")

		// Another replica commits a provider that maps the role while the delete
		// is already blocked on the key.
		insertProviderMappingTx(t, ctx, controller, "corp", "reader")
		if err := controller.Commit(); err != nil {
			t.Fatal(err)
		}

		if err := awaitResult(t, result); !errors.Is(err, domain.ErrRoleReferencedByProvider) {
			t.Fatalf("delete after concurrent provider save = %v, want ErrRoleReferencedByProvider", err)
		}
		if _, err := metadata.Role(ctx, "reader"); err != nil {
			t.Fatalf("rejected delete removed the role: %v", err)
		}
	})

	t.Run("role delete commits first, provider save rejects", func(t *testing.T) {
		metadata := openCompanionTestStore(t, "postgres")
		ctx := context.Background()
		mustCreateRole(t, metadata, "reader")

		controller, err := metadata.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Rollback()
		if err := lockOwnershipTx(ctx, controller, "role", "reader"); err != nil {
			t.Fatal(err)
		}

		result := make(chan error, 1)
		go func() {
			result <- metadata.SaveOIDCProvider(ctx, OIDCSave{
				Provider: domain.OIDCProvider{
					Name:         "corp",
					Issuer:       "https://issuer.example.com",
					ClientID:     "client",
					DefaultRoles: []string{"reader"},
				},
				Create: true,
			})
		}()
		waitForRoleKeyWaiter(t, metadata, "reader")

		// Another replica deletes the mapped role and commits while the provider
		// save is already blocked on the key.
		if _, err := controller.ExecContext(ctx, `DELETE FROM roles WHERE name = ?`, "reader"); err != nil {
			t.Fatal(err)
		}
		if err := controller.Commit(); err != nil {
			t.Fatal(err)
		}

		if err := awaitResult(t, result); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("provider save after concurrent role delete = %v, want ErrNotFound", err)
		}
		if _, err := metadata.OIDCProvider(ctx, "corp"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("rejected provider save left a provider: %v", err)
		}
	})
}

func mustCreateRole(t *testing.T, metadata *SQLStore, name string) {
	t.Helper()
	if err := metadata.SaveRole(context.Background(), RoleSave{
		Role:   domain.Role{Name: name, Privileges: []string{"repository:raw:read"}},
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func insertProviderMappingTx(t *testing.T, ctx context.Context, transaction *dialectTx, providerName, role string) {
	t.Helper()
	provider := domain.OIDCProvider{
		Name:         providerName,
		Issuer:       "https://issuer.example.com",
		ClientID:     "client",
		DefaultRoles: []string{role},
	}
	normalizeOIDCProvider(&provider)
	encoded, err := encodeOIDCCollections(provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOIDCProviderTx(ctx, transaction, provider, encoded); err != nil {
		t.Fatal(err)
	}
}

// waitForRoleKeyWaiter blocks until a backend is waiting for the role's
// ownership advisory lock, so the interleaving is fixed by lock state rather
// than by timing. The single-argument advisory lock splits its key into
// classid (high 32 bits) and objid (low 32 bits) with objsubid 1.
func waitForRoleKeyWaiter(t *testing.T, metadata *SQLStore, role string) {
	t.Helper()
	ctx := context.Background()
	key := uint64(ownershipLockKey("role", role))
	classID := int64(key >> 32)
	objID := int64(key & 0xFFFFFFFF)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiters int
		err := metadata.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND classid::bigint = ? AND objid::bigint = ?
			   AND objsubid = 1 AND NOT granted`,
			classID, objID).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the role advisory-lock waiter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("operation under test did not return")
		return nil
	}
}
