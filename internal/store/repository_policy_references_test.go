package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestRepositoryDeletionRejectsPolicyAndWebhookReferencesAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		suffix := fmt.Sprint(time.Now().UnixNano())
		repository := "referenced-" + suffix
		other := "other-" + suffix
		for _, name := range []string{repository, other} {
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
		}
		policy := domain.CleanupPolicy{
			Name: "policy-" + suffix, Repositories: []string{other, repository},
			Criteria: domain.CleanupCriteria{{Path: "classification.label", Op: "=", Value: "old"}},
		}
		if err := metadata.CreateCleanupPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
		webhook := domain.Webhook{
			Name: "hook-" + suffix, URL: "https://example.com/hook",
			Secret: "sufficiently-long-secret", Events: []string{domain.WebhookAssetUploaded},
			Repositories: []string{repository}, Enabled: true,
		}
		if err := metadata.CreateWebhook(ctx, webhook); err != nil {
			t.Fatal(err)
		}
		for _, ownership := range []Ownership{{}, {Force: true}} {
			if err := metadata.DeleteRepository(ctx, repository, ownership); !errors.Is(err, domain.ErrRepositoryInUseByCleanupPolicy) {
				t.Fatalf("delete referenced by policy with %+v = %v", ownership, err)
			}
		}
		if _, err := metadata.Repository(ctx, repository); err != nil {
			t.Fatalf("policy conflict deleted repository: %v", err)
		}
		if err := metadata.DeleteCleanupPolicy(ctx, policy.Name, Ownership{}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteRepository(ctx, repository, Ownership{}); !errors.Is(err, domain.ErrRepositoryInUseByWebhook) {
			t.Fatalf("delete referenced by webhook = %v", err)
		}
		if err := metadata.DeleteWebhook(ctx, webhook.Name, Ownership{}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteRepository(ctx, repository, Ownership{}); err != nil {
			t.Fatalf("delete after removing references: %v", err)
		}
	})
}
