package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestRepositoryOwnershipOnBothDialects exercises the repository ownership fold:
// a repository mutation and its provisioning-ownership record are committed in
// one transaction serialized on ("repository", name), so a declarative apply
// adopts and refreshes the record, an imperative mutation of a managed
// repository is rejected without force, and a forced mutation transfers
// ownership by dropping the record.
func TestRepositoryOwnershipOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			repository := domain.Repository{Name: "app", Format: "raw", Type: "hosted", Writable: true}

			// A declarative create adopts an ownership record.
			if err := metadata.SaveRepository(ctx, RepositorySave{
				Repository: repository,
				Create:     true,
				Ownership:  Ownership{Declarative: true, Fingerprint: "fp-1"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "repository", "app"); err != nil || record.SecretFingerprint != "fp-1" {
				t.Fatalf("declarative create did not adopt record: %+v %v", record, err)
			}

			// A declarative update refreshes the fingerprint in the same transaction.
			if err := metadata.SaveRepository(ctx, RepositorySave{
				Repository: repository,
				Create:     false,
				Ownership:  Ownership{Declarative: true, Fingerprint: "fp-2"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "repository", "app"); err != nil || record.SecretFingerprint != "fp-2" {
				t.Fatalf("declarative update did not refresh record: %+v %v", record, err)
			}

			// An imperative update of a managed repository without force is rejected.
			if err := metadata.SaveRepository(ctx, RepositorySave{
				Repository: repository,
				Create:     false,
			}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative update of managed repository = %v, want ErrManaged", err)
			}

			// An imperative update with force transfers ownership: the record is dropped.
			if err := metadata.SaveRepository(ctx, RepositorySave{
				Repository: repository,
				Create:     false,
				Ownership:  Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "repository", "app"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("force update did not release ownership: %v", err)
			}

			// An imperative delete of an unmanaged repository succeeds.
			if err := metadata.DeleteRepository(ctx, "app", Ownership{}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.Repository(ctx, "app"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("delete left the repository: %v", err)
			}
		})
	}
}
