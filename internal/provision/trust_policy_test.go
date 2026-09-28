package provision

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestGroupTrustPoliciesFailBeforeAnyProvisioningMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			for _, mode := range []string{"audit", "verify-on-pull", "verify-on-push"} {
				t.Run(fmt.Sprintf("existing=%t/dryRun=%t/%s", existing, dryRun, mode), func(t *testing.T) {
					ctx := context.Background()
					metadata := newProvisionTestStore(t)
					if err := metadata.CreateRepository(ctx, domain.Repository{
						Name: "member", Format: "raw", Type: "hosted",
					}); err != nil {
						t.Fatal(err)
					}
					groupResource := `
  - kind: repository
    name: group
    spec: {format: raw, type: group, members: [member]}
`
					if existing {
						if err := metadata.CreateRepository(ctx, domain.Repository{
							Name: "group", Format: "raw", Type: "group", Members: []string{"member"},
						}); err != nil {
							t.Fatal(err)
						}
						groupResource = ""
					}
					document := mustResolveDocument(t, fmt.Sprintf(`
resources:
  - kind: role
    name: untouched
    spec: {privileges: [repository:member:read]}
%s
  - kind: trustPolicy
    name: group
    spec: {mode: %s, publicKeys: [test-key]}
`, groupResource, mode))
					_, err := (Engine{Store: metadata}).Apply(ctx, document, Options{DryRun: dryRun})
					if !errors.Is(err, domain.ErrInvalidTrustPolicy) {
						t.Fatalf("preflight error = %v, want invalid trust policy", err)
					}
					if _, err := metadata.Role(ctx, "untouched"); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("unrelated role was mutated: %v", err)
					}
					if _, err := metadata.TrustPolicy(ctx, "group"); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("rejected group policy was stored: %v", err)
					}
					if !existing {
						if _, err := metadata.Repository(ctx, "group"); !errors.Is(err, domain.ErrNotFound) {
							t.Fatalf("group was created before preflight completed: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestProvisioningRejectsGroupConversionWithTrustPolicy(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	for _, name := range []string{"member", "convert"} {
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: name, Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: "convert", Mode: "audit", PublicKeys: []string{"test-key"},
	}); err != nil {
		t.Fatal(err)
	}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: untouched
    spec: {privileges: [repository:member:read]}
  - kind: repository
    name: convert
    spec: {type: group, members: [member]}
`)
	_, err := (Engine{Store: metadata}).Apply(ctx, document, Options{})
	if !errors.Is(err, domain.ErrInvalidTrustPolicy) {
		t.Fatalf("preflight error = %v, want invalid trust policy", err)
	}
	if _, err := metadata.Role(ctx, "untouched"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unrelated role was mutated: %v", err)
	}
}
