package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/cargo"
)

func TestSQLiteTrustPolicyInstanceDefaults(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}

	// No repository policy and no default: nothing applies.
	if _, err := metadata.EffectiveTrustPolicy(ctx, "raw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("effective policy without any layer = %v, want not found", err)
	}
	if _, err := metadata.TrustPolicyDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unset default should report not found, got %v", err)
	}

	// With only a default, the repository inherits it.
	if err := metadata.SetTrustPolicyDefaults(ctx, domain.TrustPolicy{
		Mode:       "audit",
		PublicKeys: []string{"instance-default-key"},
	}); err != nil {
		t.Fatal(err)
	}
	inherited, err := metadata.EffectiveTrustPolicy(ctx, "raw")
	if err != nil || inherited.Mode != "audit" || len(inherited.PublicKeys) != 1 {
		t.Fatalf("inherited policy = %+v err=%v", inherited, err)
	}

	// A repository policy overrides the default entirely.
	if err := metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: "raw",
		Mode:       "verify-on-pull",
		PublicKeys: []string{"repository-key"},
	}); err != nil {
		t.Fatal(err)
	}
	overriding, err := metadata.EffectiveTrustPolicy(ctx, "raw")
	if err != nil || overriding.Mode != "verify-on-pull" || overriding.PublicKeys[0] != "repository-key" {
		t.Fatalf("overriding policy = %+v err=%v", overriding, err)
	}

	// Removing the repository policy reverts to the inherited default.
	if err := metadata.DeleteTrustPolicy(ctx, "raw", Ownership{}); err != nil {
		t.Fatal(err)
	}
	reverted, err := metadata.EffectiveTrustPolicy(ctx, "raw")
	if err != nil || reverted.Mode != "audit" {
		t.Fatalf("reverted policy = %+v err=%v", reverted, err)
	}

	// A second write replaces the singleton default rather than inserting a row.
	if err := metadata.SetTrustPolicyDefaults(ctx, domain.TrustPolicy{
		Mode:       "verify-on-pull",
		PublicKeys: []string{"replacement-key"},
	}); err != nil {
		t.Fatal(err)
	}
	replaced, err := metadata.TrustPolicyDefaults(ctx)
	if err != nil || replaced.Mode != "verify-on-pull" || replaced.Repository != "" {
		t.Fatalf("replaced default = %+v err=%v", replaced, err)
	}

	if err := metadata.DeleteTrustPolicyDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.EffectiveTrustPolicy(ctx, "raw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("effective policy after delete = %v, want not found", err)
	}
	if err := metadata.DeleteTrustPolicyDefaults(ctx, Ownership{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleting an absent default should report not found, got %v", err)
	}
}

func TestVerifyOnPushRejectsFormatsWithoutSignatureTransport(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "raw", Format: "raw", Type: "hosted"},
		{Name: "cargo", Format: "cargo", Type: "hosted"},
		{Name: "proxy", Format: "raw", Type: "proxy", Upstream: "https://packages.example"},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	policy := domain.TrustPolicy{Mode: "verify-on-push", PublicKeys: []string{"test-key"}}
	policy.Repository = "raw"
	if err := metadata.SetTrustPolicy(ctx, policy); err != nil {
		t.Fatalf("raw verify-on-push was rejected: %v", err)
	}
	for _, repository := range []string{"cargo", "proxy"} {
		policy.Repository = repository
		if err := metadata.SetTrustPolicy(ctx, policy); !errors.Is(err, domain.ErrInvalidTrustPolicy) {
			t.Fatalf("%s verify-on-push error = %v, want invalid policy", repository, err)
		}
	}
	policy.Repository = ""
	if err := metadata.SetTrustPolicyDefaults(ctx, policy); !errors.Is(err, domain.ErrInvalidTrustPolicy) {
		t.Fatalf("global verify-on-push error = %v, want invalid policy", err)
	}
}

func TestGroupRejectsTrustPoliciesAndConversionWithPolicy(t *testing.T) {
	metadata := openMigratedSQLite(t)
	testGroupTrustPolicyConstraints(t, metadata, "")
}

func testGroupTrustPolicyConstraints(t *testing.T, metadata *SQLStore, suffix string) {
	t.Helper()
	ctx := context.Background()
	member, group, convert := "member"+suffix, "group"+suffix, "convert"+suffix
	for _, repository := range []domain.Repository{
		{Name: member, Format: "raw", Type: "hosted"},
		{Name: group, Format: "raw", Type: "group", Members: []string{member}},
		{Name: convert, Format: "raw", Type: "hosted"},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"audit", "verify-on-pull", "verify-on-push"} {
		err := metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
			Repository: group, Mode: mode, PublicKeys: []string{"test-key"},
		})
		if !errors.Is(err, domain.ErrInvalidTrustPolicy) {
			t.Fatalf("group %s policy error = %v, want invalid trust policy", mode, err)
		}
	}
	if _, err := metadata.TrustPolicy(ctx, group); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rejected group policy was stored: %v", err)
	}
	// Type is immutable, so a hosted repository can never be converted to a
	// group; the rejection is on the type change itself, independent of any
	// trust policy the repository carries.
	if err := metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: convert, Mode: "audit", PublicKeys: []string{"test-key"},
	}); err != nil {
		t.Fatal(err)
	}
	converted := domain.Repository{Name: convert, Format: "raw", Type: "group", Members: []string{member}}
	if err := metadata.UpdateRepository(ctx, converted); !errors.Is(err, domain.ErrImmutableRepositoryField) {
		t.Fatalf("conversion with policy error = %v, want immutable repository field", err)
	}
	unchanged, err := metadata.Repository(ctx, convert)
	if err != nil || unchanged.Type != "hosted" {
		t.Fatalf("rejected conversion changed repository: %+v, %v", unchanged, err)
	}
	if err := metadata.DeleteTrustPolicy(ctx, convert, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpdateRepository(ctx, converted); !errors.Is(err, domain.ErrImmutableRepositoryField) {
		t.Fatalf("conversion after removing policy error = %v, want immutable repository field", err)
	}
}
