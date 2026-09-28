package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestOverlayReplacesExplicitContainersAndPreservesInternalFields(t *testing.T) {
	current := domain.BlobStore{
		Driver:           "fs",
		PhysicalIdentity: "physical-id",
		ConfigurationRef: &domain.ConfigurationReference{Env: "OLD_CONFIG"},
		Attributes: map[string]any{
			"old":    "removed",
			"nested": map[string]any{"before": "kept in source"},
		},
	}
	desired := current
	if err := overlaySpec(map[string]any{
		"configurationRef": map[string]any{"file": "/new/config"},
		"attributes":       map[string]any{"nested": map[string]any{"after": "new"}},
	}, &desired); err != nil {
		t.Fatal(err)
	}
	if desired.Driver != "fs" || desired.PhysicalIdentity != "physical-id" {
		t.Fatalf("omitted or internal fields were lost: %+v", desired)
	}
	if desired.ConfigurationRef.Env != "" || desired.ConfigurationRef.File != "/new/config" {
		t.Fatalf("configuration object retained an omitted field: %+v", desired.ConfigurationRef)
	}
	if len(desired.Attributes) != 1 || len(desired.Attributes["nested"].(map[string]any)) != 1 {
		t.Fatalf("attributes retained removed keys: %+v", desired.Attributes)
	}
	if current.ConfigurationRef.Env != "OLD_CONFIG" || current.Attributes["old"] != "removed" {
		t.Fatalf("overlay changed source: %+v", current)
	}
	desired.Attributes["nested"].(map[string]any)["after"] = "changed"
	if _, exists := current.Attributes["nested"].(map[string]any)["after"]; exists {
		t.Fatal("overlay aliases source nested map")
	}
}

func TestOverlayReplacesPredicateSliceElements(t *testing.T) {
	current := domain.DownloadGate{Enabled: true, Criteria: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}}}
	desired := current
	if err := overlaySpec(map[string]any{"criteria": []any{map[string]any{"path": "scan.status", "op": "exists"}}}, &desired); err != nil {
		t.Fatal(err)
	}
	if !desired.Enabled || len(desired.Criteria) != 1 || desired.Criteria[0].Value != nil {
		t.Fatalf("replacement inherited omitted fields: %+v", desired)
	}
	if current.Criteria[0].Value != "passed" {
		t.Fatalf("overlay changed source: %+v", current.Criteria)
	}
}

func TestOverlayExplicitNullClearsOptionalFields(t *testing.T) {
	current := domain.BlobStore{
		Driver:           "fs",
		PhysicalIdentity: "physical-id",
		ConfigurationRef: &domain.ConfigurationReference{Env: "OLD_CONFIG"},
		Attributes:       map[string]any{"old": "value"},
	}
	desired := current
	if err := overlaySpec(map[string]any{"configurationRef": nil, "attributes": nil}, &desired); err != nil {
		t.Fatal(err)
	}
	if desired.ConfigurationRef != nil || desired.Attributes != nil || desired.Driver != "fs" || desired.PhysicalIdentity != "physical-id" {
		t.Fatalf("explicit null or omitted fields decoded incorrectly: %+v", desired)
	}
	if current.ConfigurationRef.Env != "OLD_CONFIG" || current.Attributes["old"] != "value" {
		t.Fatalf("overlay changed source: %+v", current)
	}
}

func TestOverlayKeepsOmittedNonNilEmptyField(t *testing.T) {
	current := domain.BlobStore{Driver: "fs", Attributes: map[string]any{}}
	desired := current
	if err := overlaySpec(map[string]any{"driver": "fs"}, &desired); err != nil {
		t.Fatal(err)
	}
	if desired.Attributes == nil {
		t.Fatal("omitted empty attributes map became nil")
	}
	desired.Attributes["new"] = "value"
	if len(current.Attributes) != 0 {
		t.Fatalf("preserved empty map aliases current: %+v", current.Attributes)
	}
}

func TestOverlayRejectsNoncanonicalTopLevelField(t *testing.T) {
	desired := domain.DownloadGate{Enabled: true}
	if err := overlaySpec(map[string]any{"Enabled": false}, &desired); err == nil {
		t.Fatal("case alias was accepted instead of overriding the canonical field")
	}
	if !desired.Enabled {
		t.Fatal("rejected overlay changed the destination")
	}
	if err := validateResourceSpec(Resource{Kind: "downloadGate", Name: "raw", Spec: map[string]any{"Enabled": false}}); err == nil {
		t.Fatal("document validation accepted a noncanonical field")
	}
}

func TestProvisionOIDCEmptyGroupRolesRevokesExistingMappings(t *testing.T) {
	ctx := context.Background()
	store := newProvisionTestStore(t)
	engine := Engine{Store: store}
	seed := mustResolveDocument(t, `resources:
- kind: role
  name: reviewers
  spec: {privileges: ["repository:raw:read"]}
- kind: oidcProvider
  name: corporate
  spec:
    issuer: https://issuer.example.test
    clientId: suxen
    groupRoles: {old-team: [reviewers]}
`)
	report, err := engine.Apply(ctx, seed, Options{})
	if err != nil || report.Failed() {
		t.Fatalf("seed: %+v %v", report, err)
	}
	update := mustResolveDocument(t, `resources:
- kind: oidcProvider
  name: corporate
  spec: {groupRoles: {}}
`)
	report, err = engine.Apply(ctx, update, Options{})
	if err != nil || report.Failed() {
		t.Fatalf("update: %+v %v", report, err)
	}
	if report.Results[0].Status != StatusUpdated {
		t.Fatalf("revocation status = %s", report.Results[0].Status)
	}
	provider, err := store.OIDCProvider(ctx, "corporate")
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.GroupRoles) != 0 || provider.Issuer != "https://issuer.example.test" {
		t.Fatalf("group mappings survived or omitted field lost: %+v", provider)
	}
	report, err = engine.Apply(ctx, update, Options{})
	if err != nil || report.Failed() || report.Results[0].Status != StatusUnchanged {
		t.Fatalf("repeat update: %+v %v", report, err)
	}
}

func TestProvisionPredicateReplacementClearsOldValue(t *testing.T) {
	ctx := context.Background()
	store := newProvisionTestStore(t)
	if err := store.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDownloadGate(ctx, domain.DownloadGate{Repository: "raw", Enabled: true, Criteria: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}}}); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(strings.NewReader("resources:\n- kind: downloadGate\n  name: raw\n  spec:\n    criteria:\n    - path: scan.status\n      op: exists\n"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := (Engine{Store: store}).Apply(ctx, doc, Options{})
	if err != nil || report.Failed() {
		t.Fatalf("apply: %+v %v", report, err)
	}
	gate, err := store.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.Criteria) != 1 || gate.Criteria[0].Op != "exists" || gate.Criteria[0].Value != nil || !gate.Enabled {
		t.Fatalf("old predicate value survived: %+v", gate)
	}
}
