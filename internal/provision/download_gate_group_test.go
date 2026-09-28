package provision

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestGroupDownloadGatesFailBeforeProvisioningMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/dryRun=%t", existing, dryRun), func(t *testing.T) {
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
  - kind: downloadGate
    name: group
    spec: {enabled: true, criteria: [{path: scan.status, op: '=', value: passed}]}
`, groupResource))
				_, err := (Engine{Store: metadata}).Apply(ctx, document, Options{DryRun: dryRun})
				if !errors.Is(err, domain.ErrInvalidDownloadGate) {
					t.Fatalf("preflight error = %v, want ErrInvalidDownloadGate", err)
				}
				if _, err := metadata.Role(ctx, "untouched"); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("unrelated role was mutated: %v", err)
				}
				if _, err := metadata.DownloadGate(ctx, "group"); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("rejected group gate was stored: %v", err)
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
