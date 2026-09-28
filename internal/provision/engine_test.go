package provision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type testBlobStoreController struct {
	preflightError error
	preflightCalls int
	deleteCalls    []string
}

type failingUserUpdateStore struct {
	store.Store
}

func (failingUserUpdateStore) SaveUser(context.Context, store.UserSave) error {
	return errors.New("injected user update failure")
}

// recordDeleteCountingStore counts calls to the generic DeleteProvisionRecord so
// a test can assert prune does not remove a migrated kind's ownership record a
// second time after the atomic delete command already committed its removal.
type recordDeleteCountingStore struct {
	store.Store
	genericRecordDeletes int
}

func (s *recordDeleteCountingStore) DeleteProvisionRecord(ctx context.Context, kind, name string) error {
	s.genericRecordDeletes++
	return s.Store.DeleteProvisionRecord(ctx, kind, name)
}

// TestPruneDoesNotDoubleDeleteMigratedRecord covers the double-delete race: a
// migrated kind's delete removes the resource and its ownership record in one
// transaction, so prune must not call the generic record delete afterward, which
// could strip a record another replica created and adopted for the same name.
func TestPruneDoesNotDoubleDeleteMigratedRecord(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	managed := mustResolveDocument(t, `
resources:
  - kind: user
    name: bot
    spec:
      password: bot-password-123
      roles: []
`)
	if report, err := (Engine{Store: metadata}).Apply(ctx, managed, Options{}); err != nil || report.Failed() {
		t.Fatalf("provision: report=%+v err=%v", report, err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "user", "bot"); err != nil {
		t.Fatalf("user not adopted: %v", err)
	}

	counting := &recordDeleteCountingStore{Store: metadata}
	empty := mustResolveDocument(t, "resources: []")
	report, err := (Engine{Store: counting}).Apply(ctx, empty, Options{Prune: true})
	if err != nil || report.Failed() {
		t.Fatalf("prune: report=%+v err=%v", report, err)
	}
	if _, err := metadata.User(ctx, "bot"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prune did not delete the user: %v", err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "user", "bot"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prune left the ownership record: %v", err)
	}
	if counting.genericRecordDeletes != 0 {
		t.Fatalf("generic DeleteProvisionRecord called %d times; the atomic delete already removed the record", counting.genericRecordDeletes)
	}
}

// TestPruneClearsRecordAfterCascadeDelete covers the orphan-record race: a
// managed named resource whose row was removed by a cascade (deleting its parent
// repository) still leaves its ownership record. Prune must clear that record
// rather than treat the missing row as success and skip the record delete.
func TestPruneClearsRecordAfterCascadeDelete(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	trustDoc := mustResolveDocument(t, fmt.Sprintf(`
resources:
  - kind: trustPolicy
    name: raw
    spec:
      mode: audit
      publicKeys: [%q]
`, testTrustPublicKeyPEM))
	if report, err := (Engine{Store: metadata}).Apply(ctx, trustDoc, Options{}); err != nil || report.Failed() {
		t.Fatalf("provision trust: report=%+v err=%v", report, err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "trustPolicy", "raw"); err != nil {
		t.Fatalf("trust policy not adopted: %v", err)
	}

	// Deleting the repository cascades the trust_policies row but leaves the
	// separate ownership record.
	if err := metadata.DeleteRepository(ctx, "raw", store.Ownership{Force: true}); err != nil {
		t.Fatal(err)
	}

	empty := mustResolveDocument(t, "resources: []")
	report, err := (Engine{Store: metadata}).Apply(ctx, empty, Options{Prune: true})
	if err != nil || report.Failed() {
		t.Fatalf("prune: report=%+v err=%v", report, err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "trustPolicy", "raw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prune left an orphan ownership record: %v", err)
	}
}

// TestPruneSkipsTransferredResource covers the truthful-status finding: when a
// migrated resource's ownership was transferred away (its record already gone)
// since the prune plan was captured, prune must report StatusSkipped and leave
// the resource in place rather than report a delete it did not perform.
func TestPruneSkipsTransferredResource(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.SaveUser(ctx, store.UserSave{
		Username: "bot", Password: "bot-password-123", Create: true,
		Ownership: store.Ownership{Declarative: true},
	}); err != nil {
		t.Fatal(err)
	}
	// A forced imperative update transfers ownership away after the plan captured
	// the record.
	if err := metadata.SaveUser(ctx, store.UserSave{
		Username: "bot", Admin: true, Ownership: store.Ownership{Force: true},
	}); err != nil {
		t.Fatal(err)
	}

	status, err := (Engine{Store: metadata}).deleteManagedResource(
		ctx, store.ProvisionRecord{Kind: "user", Name: "bot"}, false,
	)
	if err != nil {
		t.Fatalf("prune delete: %v", err)
	}
	if status != StatusSkipped {
		t.Fatalf("prune status = %q, want %s", status, StatusSkipped)
	}
	if _, err := metadata.User(ctx, "bot"); err != nil {
		t.Fatalf("prune removed a transferred resource: %v", err)
	}
}

// TestPruneSkipsTransferredRepository covers the destructive-prune finding for a
// kind whose delete is not folded into an ownership transaction (repository,
// role, blobStore): when a force=true API mutation transfers ownership away
// (removing the record) after the prune plan captured it, prune must re-check the
// record and leave the resource intact rather than destroy an API-owned one.
func TestPruneSkipsTransferredRepository(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "raw", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	// The prune plan captured this record.
	if err := metadata.PutProvisionRecord(ctx, store.ProvisionRecord{Kind: "repository", Name: "raw"}); err != nil {
		t.Fatal(err)
	}
	// A forced imperative mutation then transferred ownership away.
	if err := metadata.DeleteProvisionRecord(ctx, "repository", "raw"); err != nil {
		t.Fatal(err)
	}

	status, err := (Engine{Store: metadata}).deleteManagedResource(
		ctx, store.ProvisionRecord{Kind: "repository", Name: "raw"}, false,
	)
	if err != nil {
		t.Fatalf("prune delete: %v", err)
	}
	if status != StatusSkipped {
		t.Fatalf("prune status = %q, want %s", status, StatusSkipped)
	}
	if _, err := metadata.Repository(ctx, "raw"); err != nil {
		t.Fatalf("prune destroyed a transferred repository: %v", err)
	}
}

func TestEngineDoesNotPruneAfterApplyFailure(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	initial := mustResolveDocument(t, `
resources:
  - kind: role
    name: retained
    spec:
      privileges: []
  - kind: user
    name: automation
    spec:
      password: initial-password
      roles: [retained]
`)
	if report, err := (Engine{Store: metadata}).Apply(ctx, initial, Options{}); err != nil || report.Failed() {
		t.Fatalf("initial apply: report=%+v err=%v", report, err)
	}
	desired := mustResolveDocument(t, `
resources:
  - kind: user
    name: automation
    spec:
      password: initial-password
      roles: []
`)
	report, err := (Engine{Store: failingUserUpdateStore{metadata}}).Apply(ctx, desired, Options{Prune: true})
	if err != nil || !report.Failed() || len(report.Results) != 1 || report.Results[0].Status != StatusFailed {
		t.Fatalf("failed apply report=%+v err=%v", report, err)
	}
	if _, err := metadata.Role(ctx, "retained"); err != nil {
		t.Fatalf("failed apply pruned role: %v", err)
	}
	roles, err := metadata.UserRoles(ctx, "automation")
	if err != nil || len(roles) != 1 || roles[0] != "retained" {
		t.Fatalf("failed apply changed assignments: roles=%v err=%v", roles, err)
	}
}

func (controller *testBlobStoreController) ReconcileBlobStore(
	_ context.Context,
	_ domain.BlobStore,
	_ controlplane.Intent,
	dryRun bool,
) (string, bool, error) {
	if dryRun {
		controller.preflightCalls++
		if controller.preflightError != nil {
			return "", false, controller.preflightError
		}
	}
	return StatusCreated, false, nil
}

func (controller *testBlobStoreController) DeleteBlobStore(
	_ context.Context,
	name string,
	_ controlplane.Intent,
	dryRun bool,
) error {
	if !dryRun {
		controller.deleteCalls = append(controller.deleteCalls, name)
	}
	return nil
}

func TestEngineIsIdempotentAndRestoresDriftedSecret(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: user
    name: automation
    spec:
      password: declarative-password
      admin: false
      roles: []
`)

	first, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, first, StatusCreated)
	if _, authenticated := metadata.AuthenticatePassword(ctx, "automation", "declarative-password"); !authenticated {
		t.Fatal("declarative password did not authenticate")
	}

	second, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, second, StatusUnchanged)

	if err := metadata.UpdateUser(ctx, "automation", "interactive-password", false); err != nil {
		t.Fatal(err)
	}
	if _, authenticated := metadata.AuthenticatePassword(ctx, "automation", "interactive-password"); !authenticated {
		t.Fatal("interactive password did not authenticate")
	}

	restored, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, restored, StatusUpdated)
	if _, authenticated := metadata.AuthenticatePassword(ctx, "automation", "declarative-password"); !authenticated {
		t.Fatal("reconciliation did not restore the declarative password")
	}
}

func TestEngineRejectsProxyUpstreamEndpointChange(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	create := mustResolveDocument(t, `
resources:
  - kind: repository
    name: mirror
    spec:
      format: raw
      type: proxy
      secret: https://origin.example.test/raw
`)
	if _, err := engine.Apply(ctx, create, Options{}); err != nil {
		t.Fatal(err)
	}

	// Rotating only the credentials keeps the endpoint: reconciliation succeeds.
	rotate := mustResolveDocument(t, `
resources:
  - kind: repository
    name: mirror
    spec:
      format: raw
      type: proxy
      secret: https://user:secret@origin.example.test/raw
`)
	if _, err := engine.Apply(ctx, rotate, Options{}); err != nil {
		t.Fatalf("credential rotation via provisioning rejected: %v", err)
	}

	// Repointing the endpoint is reported as an immutable-field conflict without
	// deleting or recreating the proxy; the stored endpoint stays put.
	repoint := mustResolveDocument(t, `
resources:
  - kind: repository
    name: mirror
    spec:
      format: raw
      type: proxy
      secret: https://elsewhere.example.test/raw
`)
	report, err := engine.Apply(ctx, repoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatalf("endpoint change was applied, want a preflight failure: %+v", report)
	}
	rejected := false
	for _, result := range report.Results {
		if result.Status == StatusFailed &&
			strings.Contains(result.Error, "cannot be changed after creation") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("report lacks an immutable-field failure: %+v", report)
	}
	stored, err := metadata.Repository(ctx, "mirror")
	if err != nil {
		t.Fatal(err)
	}
	same, err := domain.SameUpstreamEndpoint(stored.Upstream, "https://origin.example.test/raw")
	if err != nil || !same {
		t.Fatalf("stored upstream endpoint changed to %q after rejected reconcile", stored.Upstream)
	}
}

func TestEngineProvisionedDownloadGateInheritsByDefault(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:      "raw",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: downloadGate
    name: raw
    spec:
      criteria:
        - {path: scan.status, op: "=", value: passed}
      enabled: true
`)
	report, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, report, StatusCreated)

	gate, err := metadata.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if !gate.InheritGlobal {
		t.Fatalf("a provisioned gate should inherit the instance default: %+v", gate)
	}

	second, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, second, StatusUnchanged)
}

func TestEngineProvisionedClassificationInheritsByDefault(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:      "raw",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: classification
    name: raw
    spec:
      rules:
        - {when: [], key: tier, value: public}
`)
	report, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, report, StatusCreated)

	config, err := metadata.Classification(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if !config.InheritGlobal {
		t.Fatalf("a provisioned classification should inherit the instance default: %+v", config)
	}

	second, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, second, StatusUnchanged)
}

func TestEngineDryRunAndExplicitPrune(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: publisher
    spec:
      description: Publishes artifacts
      privileges: [repository:raw:write]
`)

	dryRun, err := engine.Apply(ctx, document, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, dryRun, StatusCreated)
	if _, err := metadata.Role(ctx, "publisher"); err == nil {
		t.Fatal("dry run created a role")
	}

	if _, err := engine.Apply(ctx, document, Options{}); err != nil {
		t.Fatal(err)
	}
	empty := Document{APIVersion: APIVersion, Resources: []Resource{}}
	withoutPrune, err := engine.Apply(ctx, empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutPrune.Results) != 0 {
		t.Fatalf("results without prune = %+v", withoutPrune.Results)
	}
	if _, err := metadata.Role(ctx, "publisher"); err != nil {
		t.Fatalf("default reconciliation pruned the role: %v", err)
	}

	pruned, err := engine.Apply(ctx, empty, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, pruned, StatusDeleted)
	if _, err := metadata.Role(ctx, "publisher"); err == nil {
		t.Fatal("explicit prune retained the managed role")
	}
}

func TestEngineDefaultsSeedMissingResourcesWithoutRevertingExistingState(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	defaults := []Resource{
		{
			Kind: "role",
			Name: "anonymous",
			Spec: map[string]any{
				"description": "Unauthenticated requests",
				"privileges":  []string{},
			},
		},
	}
	engine := Engine{Store: metadata, Defaults: defaults}
	empty := Document{APIVersion: APIVersion}

	created, err := engine.Apply(ctx, empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, created, StatusCreated)

	public := mustResolveDocument(t, `
resources:
  - kind: role
    name: anonymous
    spec:
      privileges: [repository:public:read]
`)
	updated, err := engine.Apply(ctx, public, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, updated, StatusUpdated)

	unchanged, err := engine.Apply(ctx, empty, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Results) != 0 {
		t.Fatalf("unrelated apply results = %+v, want no built-in reconciliation", unchanged.Results)
	}
	role, err := metadata.Role(ctx, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(role.Privileges, []string{"repository:public:read"}) {
		t.Fatalf("anonymous privileges = %v, want explicit public read", role.Privileges)
	}

	if err := metadata.DeleteProvisionRecord(ctx, "role", "anonymous"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, empty, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "role", "anonymous"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("force-transferred built-in ownership was recreated: %v", err)
	}

	if err := metadata.DeleteRole(ctx, "anonymous", store.Ownership{}); err != nil {
		t.Fatal(err)
	}
	recreated, err := engine.Apply(ctx, empty, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSingleStatus(t, recreated, StatusCreated)
	role, err = metadata.Role(ctx, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if len(role.Privileges) != 0 {
		t.Fatalf("recreated anonymous privileges = %v, want private default", role.Privileges)
	}
}

func TestEngineRejectsNestedRolesButToleratesEmpty(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	// A nonempty includedRoles is rejected while parsing the document, before
	// any resource is applied, rather than being silently dropped.
	_, err := Parse(strings.NewReader(`
resources:
  - kind: role
    name: parent
    spec:
      privileges: []
      includedRoles: [child]
  - kind: role
    name: child
    spec:
      privileges: [repository:raw:read]
`))
	if err == nil || !strings.Contains(err.Error(), domain.ErrNestedRolesUnsupported.Error()) {
		t.Fatalf("nested role not rejected: %v", err)
	}
	if _, err := metadata.Role(ctx, "child"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rejection still mutated: %v", err)
	}

	// An empty includedRoles is tolerated for transitional documents.
	flat := mustResolveDocument(t, `
resources:
  - kind: role
    name: reader
    spec:
      privileges: [repository:raw:read]
      includedRoles: []
`)
	flatReport, err := engine.Apply(ctx, flat, Options{})
	if err != nil {
		t.Fatalf("flat role with empty includedRoles rejected: %v", err)
	}
	if flatReport.Failed() {
		t.Fatalf("flat role apply failed: %+v", flatReport.Results)
	}
	if _, err := metadata.Role(ctx, "reader"); err != nil {
		t.Fatalf("flat role not created: %v", err)
	}
}

func TestEnginePreflightsCompleteDocumentBeforeMutation(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: would-be-created
    spec:
      privileges: [repository:raw:read]
  - kind: user
    name: missing-password
    spec:
      admin: false
      roles: []
`)
	report, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatalf("report = %+v, want preflight failure", report)
	}
	if _, err := metadata.Role(ctx, "would-be-created"); err == nil {
		t.Fatal("valid resource was mutated before the complete document passed preflight")
	}
}

func TestEnginePreflightsBlobStoreReadinessBeforeAnyMutation(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	controller := &testBlobStoreController{preflightError: errors.New("driver is not ready")}
	engine := Engine{Store: metadata, BlobStores: controller}
	document := mustResolveDocument(t, `
resources:
  - kind: blobStore
    name: archive
    spec:
      driver: unavailable
      configurationRef:
        env: ARCHIVE_CONFIGURATION
  - kind: role
    name: must-not-be-created
    spec:
      privileges: [repository:*:read]
`)
	report, err := engine.Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Failed() || controller.preflightCalls != 1 {
		t.Fatalf("report = %+v, preflight calls = %d", report, controller.preflightCalls)
	}
	if _, err := metadata.Role(ctx, "must-not-be-created"); err == nil {
		t.Fatal("role was created despite failed blob-store readiness preflight")
	}
}

func TestEngineRejectsDesiredReferenceToResourceScheduledForPrune(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	roleDocument := mustResolveDocument(t, `
resources:
  - kind: role
    name: publisher
    spec:
      privileges: [repository:*:write]
`)
	if _, err := engine.Apply(ctx, roleDocument, Options{}); err != nil {
		t.Fatal(err)
	}
	userOnly := mustResolveDocument(t, `
resources:
  - kind: user
    name: automation
    spec:
      password: a-secure-password
      roles: [publisher]
`)
	_, err := engine.Apply(ctx, userOnly, Options{Prune: true})
	if err == nil || !strings.Contains(err.Error(), "scheduled for prune") {
		t.Fatalf("error = %v, want prune reference rejection", err)
	}
	if _, err := metadata.User(ctx, "automation"); err == nil {
		t.Fatal("user was created before invalid prune references were rejected")
	}
}

func TestEnginePrunesInReverseDependencyOrder(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: access
    spec:
      privileges: [repository:*:read]
  - kind: user
    name: automation
    spec:
      password: a-secure-password
      roles: [access]
`)
	if _, err := engine.Apply(ctx, document, Options{}); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Apply(
		ctx,
		Document{APIVersion: APIVersion},
		Options{Prune: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	// A subject that references a role is pruned before the role it references.
	want := []string{"user/automation", "role/access"}
	if len(report.Results) != len(want) {
		t.Fatalf("results = %+v", report.Results)
	}
	for index, result := range report.Results {
		got := result.Kind + "/" + result.Name
		if got != want[index] || result.Status != StatusDeleted {
			t.Fatalf("result %d = %+v, want %s deleted", index, result, want[index])
		}
	}
}

func TestEngineRejectsNestedStoredGroupBeforeMutation(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "member",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:    "stored-group",
		Format:  "raw",
		Type:    "group",
		Members: []string{"member"},
	}); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: repository
    name: desired-group
    spec:
      format: raw
      type: group
      members: [stored-group]
`)
	_, err := engine.Apply(ctx, document, Options{})
	if err == nil || !strings.Contains(err.Error(), "cannot itself be a group") {
		t.Fatalf("error = %v, want nested group rejection", err)
	}
	if _, err := metadata.Repository(ctx, "desired-group"); err == nil {
		t.Fatal("desired group was created before nested group validation")
	}
}

func TestEngineRejectsUnknownSpecFieldBeforeMutation(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := Document{
		APIVersion: APIVersion,
		Resources: []Resource{
			{
				Kind: "role",
				Name: "typo",
				Spec: map[string]any{
					"privilages": []string{"repository:*:read"},
				},
			},
		},
	}
	_, err := engine.Apply(ctx, document, Options{})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field rejection", err)
	}
	if _, err := metadata.Role(ctx, "typo"); err == nil {
		t.Fatal("role with unknown spec field was created")
	}
}

func TestEngineRejectsOutputOnlyAndNestedUnknownSpecFields(t *testing.T) {
	tests := []struct {
		name     string
		resource Resource
	}{
		{
			name: "path-authoritative name",
			resource: Resource{
				Kind: "role",
				Name: "reader",
				Spec: map[string]any{"name": "ignored"},
			},
		},
		{
			name: "output timestamp",
			resource: Resource{
				Kind: "role",
				Name: "reader",
				Spec: map[string]any{"createdAt": "2026-08-07T00:00:00Z"},
			},
		},
		{
			name: "nested configuration reference",
			resource: Resource{
				Kind: "blobStore",
				Name: "archive",
				Spec: map[string]any{
					"configurationRef": map[string]any{
						"env":  "ARCHIVE_STORE",
						"typo": "rejected",
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := Engine{Store: newProvisionTestStore(t)}
			document := Document{
				APIVersion: APIVersion,
				Resources:  []Resource{test.resource},
			}
			_, err := engine.Apply(context.Background(), document, Options{})
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Apply() error = %v, want strict field rejection", err)
			}
		})
	}
}

func TestEnginePreflightsOIDCUniqueIssuerBeforeMutation(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: role
    name: must-not-be-created
    spec:
      privileges: [repository:*:read]
  - kind: oidcProvider
    name: first
    spec:
      issuer: https://identity.example.test
      clientId: first-client
  - kind: oidcProvider
    name: second
    spec:
      issuer: https://identity.example.test
      clientId: second-client
`)
	_, err := engine.Apply(ctx, document, Options{})
	if err == nil || !strings.Contains(err.Error(), "same issuer") {
		t.Fatalf("error = %v, want duplicate issuer rejection", err)
	}
	if _, err := metadata.Role(ctx, "must-not-be-created"); err == nil {
		t.Fatal("role was created before OIDC uniqueness preflight")
	}
}

func TestEngineRejectsPruningReferencedBlobStore(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	blobStore := domain.BlobStore{
		Name:             "archive",
		Driver:           "test",
		ConfigurationRef: &domain.ConfigurationReference{Env: "ARCHIVE"},
		PhysicalIdentity: strings.Repeat("0", 64),
	}
	if err := metadata.CreateBlobStore(ctx, blobStore); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:      "unmanaged",
		Format:    "raw",
		Type:      "hosted",
		BlobStore: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PutProvisionRecord(ctx, store.ProvisionRecord{
		Kind: "blobStore",
		Name: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	controller := &testBlobStoreController{}
	engine := Engine{Store: metadata, BlobStores: controller}
	_, err := engine.Apply(
		ctx,
		Document{APIVersion: APIVersion},
		Options{Prune: true},
	)
	if err == nil || !strings.Contains(err.Error(), "repository \"unmanaged\" uses it") {
		t.Fatalf("error = %v, want blob-store reference rejection", err)
	}
	if len(controller.deleteCalls) != 0 {
		t.Fatalf("delete calls = %v", controller.deleteCalls)
	}
}

func newProvisionTestStore(t *testing.T) *store.SQLStore {
	t.Helper()
	metadata, err := store.OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "default",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_BLOBSTORE",
		},
		PhysicalIdentity: strings.Repeat("d", 64),
	}); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func mustResolveDocument(t *testing.T, source string) Document {
	t.Helper()
	document, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	document, err = ResolveSecrets(document, EnvironmentResolver{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func assertSingleStatus(t *testing.T, report Report, status string) {
	t.Helper()
	if len(report.Results) != 1 || report.Results[0].Status != status {
		t.Fatalf("results = %+v, want one %s", report.Results, status)
	}
}
