package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

func TestDeleteRepositoryReportsPolicyAndWebhookConflicts(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	const repository = "policy-ref"
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: repository, Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{
		Name: "policy-ref", Repositories: []string{repository},
		Criteria: domain.CleanupCriteria{{Path: "classification.label", Op: "=", Value: "old"}},
	}
	if err := fixture.Metadata.CreateCleanupPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	assertConflict := func(want string) {
		t.Helper()
		response := fixture.request(t, http.MethodDelete, "/api/v1/repositories/"+repository+"?force=true", nil, true)
		defer response.Body.Close()
		assertStatus(t, response, http.StatusConflict)
		var problem httpx.ProblemDetails
		if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		if problem.Code != want {
			t.Fatalf("problem code = %q, want %q", problem.Code, want)
		}
	}
	assertConflict("repository_in_use_by_cleanup_policy")
	if err := fixture.Metadata.DeleteCleanupPolicy(ctx, policy.Name, store.Ownership{}); err != nil {
		t.Fatal(err)
	}
	webhook := domain.Webhook{
		Name: "hook-ref", URL: "https://example.com/hook", Secret: "sufficiently-long-secret",
		Events: []string{domain.WebhookAssetUploaded}, Repositories: []string{repository}, Enabled: true,
	}
	if err := fixture.Metadata.CreateWebhook(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	assertConflict("repository_in_use_by_webhook")
	if err := fixture.Metadata.DeleteWebhook(ctx, webhook.Name, store.Ownership{}); err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodDelete, "/api/v1/repositories/"+repository, nil, true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusNoContent)
}
