package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestSQLiteRepositoryAssetAndAuthentication(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	repository := domain.Repository{Name: "releases", Format: "raw", Type: "hosted"}
	if err := metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateRepository(ctx, repository); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate repository returned %v, want ErrConflict", err)
	}

	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository:  repository.Name,
		Path:        "project/archive.tar.gz",
		Digest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:        42,
		ContentType: "application/gzip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetAttributes(
		ctx,
		repository.Name,
		asset.ID,
		"vuln.trivy",
		map[string]any{"status": "passed"},
	); err != nil {
		t.Fatal(err)
	}
	updated, err := metadata.Asset(ctx, repository.Name, asset.Path)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Attributes["vuln.trivy"] == nil {
		t.Fatal("namespaced attributes were not persisted")
	}

	if err := metadata.CreateUser(ctx, "operator", "correct horse battery staple", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.AuthenticatePassword(ctx, "operator", "wrong"); ok {
		t.Fatal("wrong password authenticated")
	}
	if user, ok := metadata.AuthenticatePassword(
		ctx,
		"operator",
		"correct horse battery staple",
	); !ok || !user.Admin {
		t.Fatal("valid administrator password did not authenticate")
	}
	if _, err := metadata.CreateToken(
		ctx,
		"operator",
		"test",
		"secret-token",
		[]string{"repository:releases:read"},
	); err != nil {
		t.Fatal(err)
	}
	tokenUser, ok := metadata.AuthenticateToken(ctx, "secret-token")
	if !ok {
		t.Fatal("valid API token did not authenticate")
	}
	if len(tokenUser.TokenScopes) != 1 {
		t.Fatalf("token scopes were not restored: %+v", tokenUser.TokenScopes)
	}
	tokens, err := metadata.Tokens(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Name != "test" {
		t.Fatalf("unexpected token metadata: %+v", tokens)
	}
	if err := metadata.DeleteToken(ctx, "operator", tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.AuthenticateToken(ctx, "secret-token"); ok {
		t.Fatal("revoked token still authenticated")
	}
}

func TestAssetReplacementCreatesNewGeneration(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "generation", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "generation", Path: "release.bin",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "generation", Path: "release.bin",
		Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("content replacement retained the previous generation ID")
	}
	if err := metadata.SetAttributes(ctx, "generation", first.ID, "scan", map[string]any{"status": "passed"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale generation annotation error = %v, want not found", err)
	}
}

func TestConcurrentAttributeNamespacesArePreserved(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "annotations", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "annotations", Path: "release.bin",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsByWriter := make(chan error, 2)
	var writers sync.WaitGroup
	for _, namespace := range []string{"scanner-a", "scanner-b"} {
		writers.Add(1)
		go func(namespace string) {
			defer writers.Done()
			<-start
			errorsByWriter <- metadata.SetAttributes(
				ctx, "annotations", asset.ID, namespace, map[string]any{"status": "passed"},
			)
		}(namespace)
	}
	close(start)
	writers.Wait()
	close(errorsByWriter)
	for err := range errorsByWriter {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, err := metadata.AssetByID(ctx, "annotations", asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"scanner-a", "scanner-b"} {
		if _, found := stored.Attributes[namespace]; !found {
			t.Fatalf("concurrent update lost namespace %q: %+v", namespace, stored.Attributes)
		}
	}
}

func TestImmutablePublicationAllowsOnlyOneDigest(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name: "packages", Format: "raw", Type: "hosted", AllowOverwrite: boolPointer(false),
	}); err != nil {
		t.Fatal(err)
	}
	digests := []string{
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	start := make(chan struct{})
	results := make(chan error, len(digests))
	var publishers sync.WaitGroup
	for _, digest := range digests {
		publishers.Add(1)
		go func(digest string) {
			defer publishers.Done()
			<-start
			_, err := metadata.PutAsset(ctx, domain.Asset{
				Repository: "packages", Path: "demo/1.0.0/package.tgz",
				Digest: digest, Immutable: true,
			})
			results <- err
		}(digest)
	}
	close(start)
	publishers.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrConflict):
			conflicted++
		default:
			t.Fatalf("publication error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("publication results: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestSQLiteNegativeCacheExpires(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:     "proxy",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://example.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	proxy, err := metadata.Repository(ctx, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := metadata.PutNegativeCacheByRepositoryID(ctx, proxy.ID, "missing", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hit, err := metadata.NegativeCacheHit(ctx, "proxy", "missing", now); err != nil || !hit {
		t.Fatalf("got hit=%v err=%v, want an active negative cache entry", hit, err)
	}
	if hit, err := metadata.NegativeCacheHit(
		ctx,
		"proxy",
		"missing",
		now.Add(2*time.Minute),
	); err != nil || hit {
		t.Fatalf("got hit=%v err=%v, want an expired negative cache entry", hit, err)
	}
}

func TestSQLiteResolvesMultipleDirectRolePrivileges(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateUser(ctx, "publisher", "password", false); err != nil {
		t.Fatal(err)
	}
	// Roles are flat: composition is at the assignment boundary, so a subject
	// that needs both privilege sets is assigned both roles directly.
	for _, role := range []domain.Role{
		{
			Name:       "raw-writer",
			Privileges: []string{"repository:raw:write"},
		},
		{
			Name:       "raw-reader",
			Privileges: []string{"repository:raw:read"},
		},
	} {
		if err := metadata.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := metadata.SetUserRoles(ctx, "publisher", []string{"raw-reader", "raw-writer"}); err != nil {
		t.Fatal(err)
	}

	privileges, err := metadata.EffectivePrivileges(ctx, "publisher")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(privileges, ",")
	if !strings.Contains(joined, "repository:raw:read") ||
		!strings.Contains(joined, "repository:raw:write") {
		t.Fatalf("direct role privileges were not resolved: %v", privileges)
	}
}

func TestSQLiteOIDCProviderLifecycle(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	provider := domain.OIDCProvider{
		Name:         "corporate",
		Issuer:       "https://identity.example/realms/engineering",
		ClientID:     "suxen",
		ClientSecret: "initial-secret",
		DefaultRoles: []string{"reader"},
		GroupRoles: map[string][]string{
			"release-engineering": {"publisher"},
		},
	}
	if err := metadata.CreateOIDCProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateOIDCProvider(ctx, provider); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate OIDC provider returned %v, want ErrConflict", err)
	}

	stored, err := metadata.OIDCProviderByIssuer(ctx, provider.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientSecret != provider.ClientSecret {
		t.Fatal("OIDC client secret was not persisted")
	}
	if stored.GroupsClaim != "groups" {
		t.Fatalf("got default groups claim %q, want groups", stored.GroupsClaim)
	}
	if len(stored.Scopes) != 4 || stored.Scopes[0] != "openid" {
		t.Fatalf("unexpected default OIDC scopes: %v", stored.Scopes)
	}

	stored.ClientID = "updated-client"
	stored.ClientSecret = "updated-secret"
	if err := metadata.UpdateOIDCProvider(ctx, stored); err != nil {
		t.Fatal(err)
	}
	updated, err := metadata.OIDCProvider(ctx, provider.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ClientID != "updated-client" || updated.ClientSecret != "updated-secret" {
		t.Fatalf("OIDC provider was not updated: %+v", updated)
	}

	providers, err := metadata.OIDCProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Name != provider.Name {
		t.Fatalf("unexpected OIDC provider list: %+v", providers)
	}
	if err := metadata.DeleteOIDCProvider(ctx, provider.Name, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.OIDCProvider(ctx, provider.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted provider returned %v, want ErrNotFound", err)
	}
}

func TestSQLiteClassificationPolicyTasksAndLeases(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "packages",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}

	beforeRules, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "packages",
		Path:       "builds/application-1.0-rc1.zip",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       1,
		Attributes: map[string]any{
			"classification": map[string]any{"source": "classifier-v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// With no rules configured, ingest assigns no classification labels, and the
	// writer-supplied classification namespace is not retained (it is rule-owned).
	if _, present := beforeRules.Attributes["classification"]; present {
		t.Fatalf("unexpected classification attributes %+v", beforeRules.Attributes)
	}
	if err := metadata.SetAttributes(
		ctx,
		"packages",
		beforeRules.ID,
		"provenance",
		map[string]any{"status": "verified"},
	); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetAttributes(
		ctx,
		"packages",
		beforeRules.ID,
		"scan",
		map[string]any{"status": "passed"},
	); err != nil {
		t.Fatal(err)
	}

	config := domain.ClassificationConfig{
		Repository: "packages",
		Rules: []domain.ClassificationRule{
			{
				When:  []domain.Predicate{{Path: "sys.path", Op: "matches", Value: `-(alpha|beta|rc\d*)\.zip$`}},
				Key:   "stage",
				Value: "prerelease",
			},
			{
				When:  []domain.Predicate{{Path: "sys.path", Op: "matches", Value: `-\d+\.\d+\.zip$`}},
				Key:   "stage",
				Value: "release",
			},
		},
	}
	reclassified, err := metadata.SetClassification(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if reclassified != 1 {
		t.Fatalf("got %d reclassified assets, want 1", reclassified)
	}
	updated, err := metadata.Asset(ctx, "packages", beforeRules.Path)
	if err != nil {
		t.Fatal(err)
	}
	if stage := classificationStage(updated.Attributes); stage != "prerelease" {
		t.Fatalf("existing asset attributes = %+v", updated.Attributes)
	}
	if updated.Attributes["provenance"] == nil || updated.Attributes["scan"] == nil {
		t.Fatalf("reclassification discarded unrelated namespaces: %+v", updated.Attributes)
	}
	release, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "packages",
		Path:       "builds/application-1.0.zip",
		Digest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Size:       1,
		Attributes: map[string]any{
			"classification": map[string]any{"stage": "forged"},
			"scan":           map[string]any{"status": "passed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage := classificationStage(release.Attributes); stage != "release" {
		t.Fatalf("new asset attributes = %+v", release.Attributes)
	}
	if release.Attributes["scan"] == nil {
		t.Fatalf("new asset lost external attributes: %+v", release.Attributes)
	}

	policy := domain.CleanupPolicy{
		Name:         "remove-prereleases",
		Repositories: []string{"packages"},
		Criteria: domain.CleanupCriteria{{
			Path: "classification.label", Op: "=", Value: "prerelease",
		}},
		Action:  "delete",
		Enabled: true,
	}
	if err := metadata.CreateCleanupPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	storedPolicy, err := metadata.CleanupPolicy(ctx, policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !storedPolicy.Enabled || len(storedPolicy.Criteria) != 1 ||
		storedPolicy.Criteria[0].Value != "prerelease" {
		t.Fatalf("unexpected stored cleanup policy: %+v", storedPolicy)
	}

	startedAt := time.Now().UTC()
	task, err := metadata.CreateTask(ctx, domain.Task{
		Type:       "cleanup",
		Status:     "running",
		Policy:     policy.Name,
		Repository: "packages",
		StartedAt:  &startedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	completedAt := startedAt.Add(time.Second)
	task.Status = "succeeded"
	task.Result = map[string]any{"deleted": float64(1)}
	task.CompletedAt = &completedAt
	if err := metadata.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	storedTask, err := metadata.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != "succeeded" || storedTask.CompletedAt == nil {
		t.Fatalf("unexpected stored task: %+v", storedTask)
	}

	now := time.Now().UTC()
	acquired, err := metadata.AcquireLease(ctx, "cleanup", "node-a", now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("first lease acquisition: acquired=%v err=%v", acquired, err)
	}
	acquired, err = metadata.AcquireLease(
		ctx,
		"cleanup",
		"node-b",
		now.Add(30*time.Second),
		now.Add(2*time.Minute),
	)
	if err != nil || acquired {
		t.Fatalf("active lease was stolen: acquired=%v err=%v", acquired, err)
	}
	acquired, err = metadata.AcquireLease(
		ctx,
		"cleanup",
		"node-b",
		now.Add(2*time.Minute),
		now.Add(3*time.Minute),
	)
	if err != nil || !acquired {
		t.Fatalf("expired lease was not acquired: acquired=%v err=%v", acquired, err)
	}
	lease, err := metadata.Lease(ctx, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "node-b" {
		t.Fatalf("got lease holder %q, want node-b", lease.Holder)
	}
}

func TestSQLiteDeleteAssetIfUnchanged(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "raw",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	asset, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "raw",
		Path:       "artifact.bin",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := metadata.DeleteAssetIfUnchanged(
		ctx,
		asset.ID,
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		asset.UpdatedAt,
	)
	if err != nil || deleted {
		t.Fatalf("changed asset was deleted: deleted=%v err=%v", deleted, err)
	}
	if _, err := metadata.Asset(ctx, "raw", asset.Path); err != nil {
		t.Fatalf("changed asset disappeared: %v", err)
	}
	deleted, err = metadata.DeleteAssetIfUnchanged(ctx, asset.ID, asset.Digest, asset.UpdatedAt)
	if err != nil || !deleted {
		t.Fatalf("unchanged asset was not deleted: deleted=%v err=%v", deleted, err)
	}
}

func TestSQLiteTracksOCIManifestReachability(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "oci",
		Format: "oci",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	layerDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifestDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci",
		Path:       "v2/example/image/blobs/" + layerDigest,
		Digest:     layerDigest,
		Kind:       "oci-blob",
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}
	manifest := domain.Asset{
		Repository:   "oci",
		Digest:       manifestDigest,
		Kind:         "oci-manifest",
		Size:         1,
		Dependencies: []string{layerDigest},
	}
	manifest.Path = "v2/example/image/manifests/latest"
	manifest.Reference = "latest"
	if _, err := metadata.PutAsset(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Path = "v2/example/image/manifests/" + manifestDigest
	manifest.Reference = manifestDigest
	if _, err := metadata.PutAsset(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	standaloneDigest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci",
		Path:       "v2/example/standalone/manifests/" + standaloneDigest,
		Digest:     standaloneDigest,
		Kind:       "oci-manifest",
		Reference:  standaloneDigest,
		Size:       1,
	}); err != nil {
		t.Fatal(err)
	}

	references, err := metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := references[layerDigest]; !found {
		t.Fatal("manifest dependency did not retain its layer")
	}
	removedTag, err := metadata.DeleteAsset(
		ctx,
		"oci",
		"v2/example/image/manifests/latest",
	)
	if err != nil {
		t.Fatal(err)
	}
	deletedAliases, err := metadata.DeleteDanglingManifestAliases(
		ctx,
		"oci",
		[]domain.Asset{removedTag},
	)
	if err != nil {
		t.Fatal(err)
	}
	if deletedAliases != 1 {
		t.Fatalf("deleted %d dangling aliases, want 1", deletedAliases)
	}
	if _, err := metadata.Asset(
		ctx,
		"oci",
		"v2/example/standalone/manifests/"+standaloneDigest,
	); err != nil {
		t.Fatalf("scoped pruning deleted a digest-only manifest: %v", err)
	}
	references, err = metadata.ReferencedDigests(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := references[layerDigest]; found {
		t.Fatal("deleted manifest still retained its layer")
	}
	deletedBlobAssets, err := metadata.DeleteUnreferencedBlobAssets(
		ctx,
		"default",
		layerDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if deletedBlobAssets != 1 {
		t.Fatalf("deleted %d orphan blob assets, want 1", deletedBlobAssets)
	}
}

func classificationStage(attributes map[string]any) string {
	classification, _ := attributes["classification"].(map[string]any)
	stage, _ := classification["stage"].(string)
	return stage
}

func boolPointer(value bool) *bool { return &value }
