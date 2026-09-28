package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestOIDCProviderOwnershipOnBothDialects exercises the same ownership decisions
// for the OIDC provider kind: declarative adoption, the imperative managed
// guard, forced transfer, and atomic delete of the provider with its record.
func TestOIDCProviderOwnershipOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			provider := domain.OIDCProvider{
				Name: "idp", Issuer: "https://issuer.example", ClientID: "suxen", ClientSecret: "secret-1",
			}

			if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
				Provider: provider, Create: true,
				Ownership: Ownership{Declarative: true, Fingerprint: "fp-1"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "oidcProvider", "idp"); err != nil || record.SecretFingerprint != "fp-1" {
				t.Fatalf("declarative create did not adopt record: %+v %v", record, err)
			}

			update := provider
			update.ClientID = "changed"
			if err := metadata.SaveOIDCProvider(ctx, OIDCSave{Provider: update}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative update of managed provider = %v, want ErrManaged", err)
			}
			if got, err := metadata.OIDCProvider(ctx, "idp"); err != nil || got.ClientID != "suxen" {
				t.Fatalf("rejected update still changed clientID: %+v %v", got, err)
			}

			if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
				Provider: update, Ownership: Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "oidcProvider", "idp"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("force update did not release ownership: %v", err)
			}
			if got, err := metadata.OIDCProvider(ctx, "idp"); err != nil || got.ClientID != "changed" {
				t.Fatalf("force update did not apply: %+v %v", got, err)
			}

			// A declarative delete of the now-unmanaged provider is skipped so a
			// stale prune plan cannot remove a resource transferred to the API.
			if err := metadata.DeleteOIDCProvider(ctx, "idp", Ownership{Declarative: true}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("declarative delete of transferred provider = %v, want ErrNotFound", err)
			}
			if _, err := metadata.OIDCProvider(ctx, "idp"); err != nil {
				t.Fatalf("declarative delete removed a transferred provider: %v", err)
			}

			// Re-adopting it declaratively lets a declarative delete remove the
			// provider and its record together.
			if err := metadata.SaveOIDCProvider(ctx, OIDCSave{
				Provider: update, Ownership: Ownership{Declarative: true, Fingerprint: "fp-2"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := metadata.DeleteOIDCProvider(ctx, "idp", Ownership{Declarative: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.OIDCProvider(ctx, "idp"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("declarative delete left the provider: %v", err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "oidcProvider", "idp"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("declarative delete left the record: %v", err)
			}
		})
	}
}

// TestDeclarativeDeleteSkipsTransferredResource covers the stale-prune race: a
// declarative (prune) delete must not remove a resource whose ownership was
// transferred away since the prune plan was captured. Once the record is gone
// the resource is API-owned, so the keyed delete reports ErrNotFound and leaves
// the resource in place instead of acting on the stale plan.
func TestDeclarativeDeleteSkipsTransferredResource(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Password: "bot-password-123", Create: true,
				Ownership: Ownership{Declarative: true, Fingerprint: "fp-1"},
			}); err != nil {
				t.Fatal(err)
			}
			// A forced imperative update transfers ownership away: the record drops.
			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Admin: true, Ownership: Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			// A prune plan captured before the transfer would delete "bot"; the
			// keyed declarative delete must instead skip it.
			if err := metadata.DeleteUser(ctx, "bot", Ownership{Declarative: true}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("stale declarative delete = %v, want ErrNotFound", err)
			}
			if _, err := metadata.User(ctx, "bot"); err != nil {
				t.Fatalf("stale declarative delete removed a transferred resource: %v", err)
			}
		})
	}
}

// TestUserOwnershipOnBothDialects exercises the ownership decisions SaveUser and
// DeleteUser make inside their own transaction: declarative adoption, the
// imperative managed-conflict guard, forced ownership transfer, and atomic
// delete of the account together with its ownership record.
func TestUserOwnershipOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()

			// A declarative create adopts an ownership record with its fingerprint.
			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Password: "bot-password-123", Create: true,
				Ownership: Ownership{Declarative: true, Fingerprint: "fp-1"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "user", "bot"); err != nil || record.SecretFingerprint != "fp-1" {
				t.Fatalf("declarative create did not adopt record: %+v %v", record, err)
			}

			// An imperative update of a managed user without force is rejected and
			// leaves the account untouched.
			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Admin: true, Create: false,
			}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative update of managed user = %v, want ErrManaged", err)
			}
			if user, err := metadata.User(ctx, "bot"); err != nil || user.Admin {
				t.Fatalf("rejected update still changed admin: %+v %v", user, err)
			}

			// An imperative update with force transfers ownership: the record is dropped.
			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Admin: true, Create: false,
				Ownership: Ownership{Force: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "user", "bot"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("force update did not release ownership: %v", err)
			}
			if user, err := metadata.User(ctx, "bot"); err != nil || !user.Admin {
				t.Fatalf("force update did not apply: %+v %v", user, err)
			}

			// A declarative apply re-adopts the now-unmanaged user.
			if err := metadata.SaveUser(ctx, UserSave{
				Username: "bot", Create: false,
				Ownership: Ownership{Declarative: true, Fingerprint: "fp-2"},
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := metadata.ProvisionRecord(ctx, "user", "bot"); err != nil || record.SecretFingerprint != "fp-2" {
				t.Fatalf("declarative re-adopt failed: %+v %v", record, err)
			}

			// An imperative delete of a managed user without force is rejected.
			if err := metadata.DeleteUser(ctx, "bot", Ownership{}); !errors.Is(err, domain.ErrManaged) {
				t.Fatalf("imperative delete of managed user = %v, want ErrManaged", err)
			}
			if _, err := metadata.User(ctx, "bot"); err != nil {
				t.Fatalf("rejected delete removed the user: %v", err)
			}

			// A declarative delete removes the account and its record together.
			if err := metadata.DeleteUser(ctx, "bot", Ownership{Declarative: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.User(ctx, "bot"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("declarative delete left the user: %v", err)
			}
			if _, err := metadata.ProvisionRecord(ctx, "user", "bot"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("declarative delete left the record: %v", err)
			}
		})
	}
}
