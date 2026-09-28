package provision

import (
	"context"
	"testing"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
)

// conflictOnceRoleCommands simulates a concurrent creator: the first create
// returns ErrConflict (another replica won the race), and the follow-up update
// succeeds. It records the calls so the recovery path can be asserted
// deterministically without relying on goroutine timing.
type conflictOnceRoleCommands struct {
	createCalls int
	updateCalls int
}

func (c *conflictOnceRoleCommands) SaveRole(_ context.Context, cmd controlplane.SaveRoleCommand) error {
	if cmd.Create {
		c.createCalls++
		return domain.ErrConflict
	}
	c.updateCalls++
	return nil
}

func (c *conflictOnceRoleCommands) DeleteRole(context.Context, string, controlplane.Intent) error {
	return nil
}

// TestReconcileRoleAdoptsConcurrentCreateWinner asserts that when a declarative
// role create loses to a concurrent creator, the reconciler adopts the winner
// through an atomic update instead of failing the apply.
func TestReconcileRoleAdoptsConcurrentCreateWinner(t *testing.T) {
	metadata := newProvisionTestStore(t)
	ctx := context.Background()

	commands := &conflictOnceRoleCommands{}
	engine := Engine{Store: metadata, Roles: commands}

	status, ownershipPersisted, err := engine.reconcileRole(
		ctx,
		Resource{Kind: "role", Name: "reader", Spec: map[string]any{"privileges": []any{"repository:raw:read"}}},
		"",
		false,
		false,
	)
	if err != nil {
		t.Fatalf("reconcileRole returned error on concurrent create: %v", err)
	}
	if !ownershipPersisted {
		t.Fatalf("reconcileRole did not persist ownership: status=%q", status)
	}
	if commands.createCalls != 1 || commands.updateCalls != 1 {
		t.Fatalf("expected one create then one adopting update, got create=%d update=%d", commands.createCalls, commands.updateCalls)
	}
}
