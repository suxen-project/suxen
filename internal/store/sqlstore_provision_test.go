package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestOIDCSecretMutationInvalidatesProvisionFingerprint(t *testing.T) {
	ctx := context.Background()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	migrateTestMetadata(t, metadata)
	provider := domain.OIDCProvider{
		Name:         "workforce",
		Issuer:       "https://identity.example.test",
		ClientID:     "suxen",
		ClientSecret: "original-client-secret",
	}
	if err := metadata.CreateOIDCProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	record := ProvisionRecord{
		Kind:              "oidcProvider",
		Name:              provider.Name,
		SecretFingerprint: "fingerprint",
		UpdatedAt:         time.Now().UTC(),
	}
	if err := metadata.PutProvisionRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	provider.ClientSecret = ""
	provider.ClientID = "updated-client"
	if err := metadata.UpdateOIDCProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	stored, err := metadata.OIDCProvider(ctx, provider.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientSecret != "original-client-secret" {
		t.Fatalf("omitted secret was overwritten: %q", stored.ClientSecret)
	}
	if _, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name); err != nil {
		t.Fatalf("non-secret update invalidated fingerprint: %v", err)
	}

	provider.ClientSecret = "original-client-secret"
	if err := metadata.UpdateOIDCProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	sameSecret, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if sameSecret.SecretFingerprint != record.SecretFingerprint {
		t.Fatalf("same secret invalidated fingerprint: %+v", sameSecret)
	}

	provider.ClientSecret = "rotated-client-secret"
	if err := metadata.UpdateOIDCProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	invalidated, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatalf("secret update removed provisioning ownership: %v", err)
	}
	if invalidated.SecretFingerprint != "" {
		t.Fatalf("invalidated record = %+v", invalidated)
	}
}

func TestUserUpdateOnlyInvalidatesFingerprintWhenPasswordChanges(t *testing.T) {
	ctx := context.Background()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateUser(ctx, "automation", "original-password", false); err != nil {
		t.Fatal(err)
	}
	record := ProvisionRecord{
		Kind:              "user",
		Name:              "automation",
		SecretFingerprint: "user-secret-fingerprint",
	}
	if err := metadata.PutProvisionRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	if err := metadata.UpdateUser(ctx, record.Name, "original-password", true); err != nil {
		t.Fatal(err)
	}
	retained, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if retained.SecretFingerprint != record.SecretFingerprint {
		t.Fatalf("same password invalidated fingerprint: %+v", retained)
	}

	if err := metadata.UpdateUser(ctx, record.Name, "rotated-password", true); err != nil {
		t.Fatal(err)
	}
	invalidated, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.SecretFingerprint != "" {
		t.Fatalf("changed password did not preserve ownership: %+v", invalidated)
	}
}

func TestRepositoryOnlyInvalidatesFingerprintWhenUpstreamChanges(t *testing.T) {
	ctx := context.Background()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	migrateTestMetadata(t, metadata)
	repository := domain.Repository{
		Name:     "upstream",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://packages.example.test/raw",
	}
	if err := metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	record := ProvisionRecord{
		Kind:              "repository",
		Name:              repository.Name,
		SecretFingerprint: "repository-secret-fingerprint",
	}
	if err := metadata.PutProvisionRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	repository.Writable = true
	if err := metadata.UpdateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	retained, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if retained.SecretFingerprint != record.SecretFingerprint {
		t.Fatalf("non-secret update invalidated fingerprint: %+v", retained)
	}

	// Rotating the embedded credentials keeps the same endpoint, so the update
	// is allowed and clears the secret fingerprint for re-detection.
	repository.Upstream = "https://rotated:secret@packages.example.test/raw"
	if err := metadata.UpdateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	invalidated, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.SecretFingerprint != "" {
		t.Fatalf("secret update did not preserve ownership: %+v", invalidated)
	}

	// Changing the endpoint itself (path) is rejected as an immutable field.
	repository.Upstream = "https://packages.example.test/oci"
	if err := metadata.UpdateRepository(ctx, repository); !errors.Is(
		err, domain.ErrImmutableRepositoryField,
	) {
		t.Fatalf("endpoint change = %v, want immutable-field error", err)
	}
}

func TestWebhookPutOnlyInvalidatesFingerprintWhenSecretChanges(t *testing.T) {
	ctx := context.Background()
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	migrateTestMetadata(t, metadata)
	webhook := domain.Webhook{
		Name:    "notifications",
		URL:     "https://hooks.example.test/initial",
		Secret:  "original-webhook-secret",
		Events:  []string{domain.WebhookAssetUploaded},
		Enabled: true,
	}
	if err := metadata.CreateWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	record := ProvisionRecord{
		Kind:              "webhook",
		Name:              webhook.Name,
		SecretFingerprint: "webhook-secret-fingerprint",
	}
	if err := metadata.PutProvisionRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	webhook.URL = "https://hooks.example.test/updated"
	webhook.Secret = ""
	if _, err := metadata.PutWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	retained, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if retained.SecretFingerprint != record.SecretFingerprint {
		t.Fatalf("non-secret update invalidated fingerprint: %+v", retained)
	}

	webhook.Secret = "original-webhook-secret"
	if _, err := metadata.PutWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	sameSecret, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if sameSecret.SecretFingerprint != record.SecretFingerprint {
		t.Fatalf("same secret invalidated fingerprint: %+v", sameSecret)
	}

	webhook.Secret = "rotated-webhook-secret"
	if _, err := metadata.PutWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	invalidated, err := metadata.ProvisionRecord(ctx, record.Kind, record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.SecretFingerprint != "" {
		t.Fatalf("secret update did not preserve ownership: %+v", invalidated)
	}
}
