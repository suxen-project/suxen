package provision

import (
	"context"
	"reflect"
	"testing"
)

func resultKeys(report Report) []string {
	keys := make([]string, len(report.Results))
	for index, result := range report.Results {
		keys[index] = result.Kind + "/" + result.Name
	}
	return keys
}

func indexOf(keys []string, key string) int {
	for index, candidate := range keys {
		if candidate == key {
			return index
		}
	}
	return -1
}

func assertBefore(t *testing.T, keys []string, earlier string, later string) {
	t.Helper()
	earlierIndex := indexOf(keys, earlier)
	laterIndex := indexOf(keys, later)
	if earlierIndex < 0 || laterIndex < 0 {
		t.Fatalf("missing key: %q at %d, %q at %d in %v", earlier, earlierIndex, later, laterIndex, keys)
	}
	if earlierIndex >= laterIndex {
		t.Fatalf("want %q before %q, got order %v", earlier, later, keys)
	}
}

// TestEngineOrdersByFixedPhasesAndIsShuffleInvariant proves the phase sort
// replaced the recursive walk without weakening ordering: every dependency lands
// in an earlier phase, groups follow their leaf members, and the applied order is
// independent of the document's input order.
func TestEngineOrdersByFixedPhasesAndIsShuffleInvariant(t *testing.T) {
	ctx := context.Background()
	resources := []Resource{
		{Kind: "role", Name: "access", Spec: map[string]any{"privileges": []string{"repository:*:read"}}},
		{Kind: "user", Name: "automation", Secret: "a-secure-password", Spec: map[string]any{"roles": []string{"access"}}},
		{Kind: "repository", Name: "alpha", Spec: map[string]any{"format": "raw", "type": "hosted"}},
		{Kind: "repository", Name: "zeta", Spec: map[string]any{"format": "raw", "type": "hosted"}},
		{Kind: "repository", Name: "bundle", Spec: map[string]any{"format": "raw", "type": "group", "members": []string{"alpha"}}},
	}

	apply := func(order []Resource) []string {
		engine := Engine{Store: newProvisionTestStore(t)}
		report, err := engine.Apply(ctx, Document{APIVersion: APIVersion, Resources: order}, Options{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if report.Failed() {
			t.Fatalf("report failed: %+v", report.Results)
		}
		return resultKeys(report)
	}

	forward := apply(resources)
	reversed := make([]Resource, len(resources))
	for index := range resources {
		reversed[index] = resources[len(resources)-1-index]
	}
	if got := apply(reversed); !reflect.DeepEqual(got, forward) {
		t.Fatalf("shuffled input changed order:\n forward  = %v\n reversed = %v", forward, got)
	}

	assertBefore(t, forward, "repository/alpha", "role/access")       // leaf repository (20) before role (30)
	assertBefore(t, forward, "role/access", "user/automation")        // role (30) before its subject (40)
	assertBefore(t, forward, "repository/alpha", "repository/bundle") // a group follows its member leaf
	assertBefore(t, forward, "repository/zeta", "repository/bundle")  // every leaf precedes groups
}

// TestEnginePhaseApplyHasDryRunParityAndIsIdempotent checks that the preflight
// pass reports the same ordered results as the real pass, and that reapplying an
// unchanged document is a stable no-op.
func TestEnginePhaseApplyHasDryRunParityAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	engine := Engine{Store: newProvisionTestStore(t)}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: access
    spec:
      privileges: [repository:*:read]
  - kind: repository
    name: alpha
    spec:
      format: raw
      type: hosted
  - kind: repository
    name: bundle
    spec:
      format: raw
      type: group
      members: [alpha]
  - kind: user
    name: automation
    spec:
      password: a-secure-password
      roles: [access]
`)

	dry, err := engine.Apply(ctx, document, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	real, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resultKeys(dry), resultKeys(real)) {
		t.Fatalf("dry-run order %v != real order %v", resultKeys(dry), resultKeys(real))
	}
	for _, result := range append(dry.Results, real.Results...) {
		if result.Status != StatusCreated {
			t.Fatalf("status %+v, want created", result)
		}
	}

	again, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resultKeys(again), resultKeys(real)) {
		t.Fatalf("idempotent order changed: %v != %v", resultKeys(again), resultKeys(real))
	}
	for _, result := range again.Results {
		if result.Status != StatusUnchanged {
			t.Fatalf("second apply status %+v, want unchanged", result)
		}
	}
}

// TestEnginePrunesGroupBeforeMemberLeaf covers reverse-phase deletion for
// repositories: a group is removed before the leaf it references, so a prune
// never deletes through a still-referenced member.
func TestEnginePrunesGroupBeforeMemberLeaf(t *testing.T) {
	ctx := context.Background()
	engine := Engine{Store: newProvisionTestStore(t)}
	document := mustResolveDocument(t, `
resources:
  - kind: repository
    name: alpha
    spec:
      format: raw
      type: hosted
  - kind: repository
    name: bundle
    spec:
      format: raw
      type: group
      members: [alpha]
`)
	if _, err := engine.Apply(ctx, document, Options{}); err != nil {
		t.Fatal(err)
	}

	report, err := engine.Apply(ctx, Document{APIVersion: APIVersion}, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	keys := resultKeys(report)
	assertBefore(t, keys, "repository/bundle", "repository/alpha")
	for _, result := range report.Results {
		if result.Status != StatusDeleted {
			t.Fatalf("prune status %+v, want deleted", result)
		}
	}
}
