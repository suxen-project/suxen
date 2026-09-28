//go:build suxen_integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestPostgresStoreIntegration(t *testing.T) {
	dataSourceName := os.Getenv("SUXEN_TEST_POSTGRES")
	if dataSourceName == "" {
		t.Skip("SUXEN_TEST_POSTGRES is not configured")
	}
	metadata, err := OpenPostgres(dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := createTestDefaultBlobStore(ctx, metadata.SQLStore); err != nil && !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	defaultStore, err := metadata.BlobStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if defaultStore.Driver != "fs" || defaultStore.ConfigurationRef == nil ||
		defaultStore.ConfigurationRef.Env != "SUXEN_BLOBSTORE" ||
		defaultStore.PhysicalIdentity != strings.Repeat("0", 64) {
		t.Fatalf("unexpected shared default blob store: %+v", defaultStore)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	t.Run("GroupTrustPolicyConstraints", func(t *testing.T) {
		testGroupTrustPolicyConstraints(t, metadata.SQLStore, "-"+suffix)
	})
	repositoryName := "pg-" + suffix
	roleName := "publisher-" + suffix
	username := "user-" + suffix
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   repositoryName,
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository: repositoryName,
		Rules: []domain.ClassificationRule{
			{
				When:  []domain.Predicate{{Path: "sys.path", Op: "contains", Value: "snapshot"}},
				Key:   "stage",
				Value: "prerelease",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: repositoryName,
		Path:       "project-snapshot.zip",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage := classificationStage(asset.Attributes); stage != "prerelease" {
		t.Fatalf("PostgreSQL asset attributes = %+v", asset.Attributes)
	}
	repoClassification, err := metadata.Classification(ctx, repositoryName)
	if err != nil || repoClassification.InheritGlobal {
		t.Fatalf("PostgreSQL classification inherit_global: config=%+v err=%v", repoClassification, err)
	}
	if _, err := metadata.SetClassificationDefaults(ctx, domain.ClassificationConfig{
		Rules: []domain.ClassificationRule{{Key: "tier", Value: "shared"}},
	}); err != nil {
		t.Fatal(err)
	}
	classificationDefaults, err := metadata.ClassificationDefaults(ctx)
	if err != nil || len(classificationDefaults.Rules) != 1 ||
		classificationDefaults.Rules[0].Key != "tier" {
		t.Fatalf("PostgreSQL classification defaults: defaults=%+v err=%v", classificationDefaults, err)
	}
	if err := metadata.DeleteClassificationDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetAttributes(
		ctx,
		repositoryName,
		asset.ID,
		"scan",
		map[string]any{"status": "passed"},
	); err != nil {
		t.Fatal(err)
	}
	asset.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	asset, err = metadata.PutAsset(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if len(asset.Attributes) != 1 {
		t.Fatalf("replacement asset retained stale scan attributes: %+v", asset.Attributes)
	}

	if err := metadata.CreateRole(ctx, domain.Role{
		Name:       roleName,
		Privileges: []string{"repository:" + repositoryName + ":write"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateUser(ctx, username, "postgres-test-password", false); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetUserRoles(ctx, username, []string{roleName}); err != nil {
		t.Fatal(err)
	}
	privileges, err := metadata.EffectivePrivileges(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(privileges, ","), repositoryName) {
		t.Fatalf("PostgreSQL role privileges were not resolved: %v", privileges)
	}
	if err := metadata.UpdateUser(ctx, username, "postgres-updated-password", true); err != nil {
		t.Fatal(err)
	}
	updatedUser, err := metadata.User(ctx, username)
	if err != nil || !updatedUser.Admin {
		t.Fatalf("PostgreSQL updated user: user=%+v err=%v", updatedUser, err)
	}
	if _, authenticated := metadata.AuthenticatePassword(
		ctx,
		username,
		"postgres-updated-password",
	); !authenticated {
		t.Fatal("PostgreSQL updated password did not authenticate")
	}

	policyName := "cleanup-" + suffix
	if err := metadata.CreateCleanupPolicy(ctx, domain.CleanupPolicy{
		Name:         policyName,
		Repositories: []string{repositoryName},
		Criteria: domain.CleanupCriteria{{
			Path: "classification.label", Op: "=", Value: "prerelease",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	task, err := metadata.CreateTask(ctx, domain.Task{
		Type:       "cleanup",
		Status:     "running",
		Policy:     policyName,
		Repository: repositoryName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == 0 {
		t.Fatal("PostgreSQL task did not return an identifier")
	}

	now := time.Now().UTC()
	acquired, err := metadata.AcquireLease(ctx, "integration-"+suffix, suffix, now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("PostgreSQL lease acquisition: acquired=%v err=%v", acquired, err)
	}

	if err := metadata.CreateWebhook(ctx, domain.Webhook{
		Name:         "postgres-hook-" + suffix,
		URL:          "https://hooks.example/suxen",
		Secret:       "postgres-integration-secret",
		Events:       []string{domain.WebhookAssetUploaded},
		Repositories: []string{repositoryName},
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.EnqueueWebhookEvent(ctx, domain.WebhookEvent{
		ID:         "postgres-event-" + suffix,
		Type:       domain.WebhookAssetUploaded,
		Repository: repositoryName,
		OccurredAt: now,
		Asset:      &asset,
	}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := metadata.ClaimWebhookDeliveries(
		ctx,
		"postgres-worker-"+suffix,
		now,
		time.Minute,
		10,
	)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("PostgreSQL webhook claim: deliveries=%+v err=%v", deliveries, err)
	}
	if err := metadata.CompleteWebhookDelivery(
		ctx,
		deliveries[0].ID,
		"postgres-worker-"+suffix,
		"delivered",
		now,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetDownloadGate(ctx, domain.DownloadGate{
		Repository: repositoryName,
		Criteria: []domain.Predicate{
			{Path: "scan.status", Op: "=", Value: "passed"},
		},
		Enabled:       true,
		InheritGlobal: true,
	}); err != nil {
		t.Fatal(err)
	}
	gate, err := metadata.DownloadGate(ctx, repositoryName)
	if err != nil || len(gate.Criteria) != 1 || gate.Criteria[0].Value != "passed" {
		t.Fatalf("PostgreSQL download gate: gate=%+v err=%v", gate, err)
	}
	if !gate.InheritGlobal {
		t.Fatalf("PostgreSQL download gate did not round-trip inherit_global: %+v", gate)
	}
	if err := metadata.SetDownloadGateDefaults(ctx, domain.DownloadGate{
		Criteria: []domain.Predicate{
			{Path: "sys.blobStore", Op: "=", Value: "default"},
		},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	defaults, err := metadata.DownloadGateDefaults(ctx)
	if err != nil || len(defaults.Criteria) != 1 || defaults.Criteria[0].Path != "sys.blobStore" {
		t.Fatalf("PostgreSQL download-gate defaults: defaults=%+v err=%v", defaults, err)
	}
	if err := metadata.DeleteDownloadGateDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: repositoryName,
		Mode:       "audit",
		PublicKeys: []string{"test-public-key"},
	}); err != nil {
		t.Fatal(err)
	}
	trustPolicy, err := metadata.TrustPolicy(ctx, repositoryName)
	if err != nil || trustPolicy.Mode != "audit" {
		t.Fatalf("PostgreSQL trust policy: policy=%+v err=%v", trustPolicy, err)
	}
	// The repository policy overrides any instance-wide default.
	if err := metadata.SetTrustPolicyDefaults(ctx, domain.TrustPolicy{
		Mode:       "verify-on-pull",
		PublicKeys: []string{"instance-default-key"},
	}); err != nil {
		t.Fatal(err)
	}
	trustDefaults, err := metadata.TrustPolicyDefaults(ctx)
	if err != nil || trustDefaults.Mode != "verify-on-pull" || trustDefaults.Repository != "" {
		t.Fatalf("PostgreSQL trust-policy defaults: defaults=%+v err=%v", trustDefaults, err)
	}
	effective, err := metadata.EffectiveTrustPolicy(ctx, repositoryName)
	if err != nil || effective.Mode != "audit" {
		t.Fatalf("PostgreSQL effective trust policy: effective=%+v err=%v", effective, err)
	}
	if err := metadata.DeleteTrustPolicyDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
}
