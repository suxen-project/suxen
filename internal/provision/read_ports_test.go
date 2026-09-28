package provision

import (
	"context"
	"testing"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
)

// countingRoleReader records how often the engine reads a role through the port
// and reports the role as absent, so a reconcile takes the create branch without
// touching the underlying store.
type countingRoleReader struct {
	calls int
}

func (r *countingRoleReader) Role(context.Context, string) (domain.Role, error) {
	r.calls++
	return domain.Role{}, domain.ErrNotFound
}

// noopRoleCommands accepts a save without persisting so the reconcile under test
// exercises only the read path.
type noopRoleCommands struct {
	saved int
}

func (c *noopRoleCommands) SaveRole(context.Context, controlplane.SaveRoleCommand) error {
	c.saved++
	return nil
}

func (c *noopRoleCommands) DeleteRole(context.Context, string, controlplane.Intent) error {
	return nil
}

// TestReconcileReadsThroughInjectedRoleReader asserts the reconciler resolves a
// role through the injected reader port rather than the store. It fails if a read
// site still calls engine.Store directly, because the spy would then never be
// consulted.
func TestReconcileReadsThroughInjectedRoleReader(t *testing.T) {
	metadata := newProvisionTestStore(t)
	ctx := context.Background()

	reader := &countingRoleReader{}
	commands := &noopRoleCommands{}
	engine := Engine{Store: metadata, RoleReads: reader, Roles: commands}

	_, ownershipPersisted, err := engine.reconcileRole(
		ctx,
		Resource{Kind: "role", Name: "reader", Spec: map[string]any{"privileges": []any{"repository:raw:read"}}},
		"",
		false,
		false,
	)
	if err != nil {
		t.Fatalf("reconcileRole returned error: %v", err)
	}
	if !ownershipPersisted {
		t.Fatalf("reconcileRole did not persist ownership through the command port")
	}
	if reader.calls == 0 {
		t.Fatalf("reconcileRole did not read the role through the injected RoleReader")
	}
	if commands.saved == 0 {
		t.Fatalf("reconcileRole did not save through the command port")
	}
}

// TestReaderPortsFallBackToStore asserts that a bare Engine (no injected reader)
// resolves reads through its store, which every reconcile test relies on.
func TestReaderPortsFallBackToStore(t *testing.T) {
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	if engine.roleReads() != RoleReader(metadata) {
		t.Fatalf("roleReads did not fall back to the store")
	}
	if engine.repositoryReads() != RepositoryReader(metadata) {
		t.Fatalf("repositoryReads did not fall back to the store")
	}
}
