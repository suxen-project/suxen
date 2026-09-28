package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestStorageUsageEndpointReportsPerRepositoryAndStore(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: "usage-repo", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "usage-repo",
		Path:       "artifact",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:       128,
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(t, http.MethodGet, "/api/v1/usage", nil, true)
	assertStatus(t, response, http.StatusOK)
	var usage store.StorageUsage
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	var repoBytes int64 = -1
	for _, row := range usage.Repositories {
		if row.Repository == "usage-repo" {
			repoBytes = row.Bytes
		}
	}
	if repoBytes != 128 {
		t.Fatalf("usage-repo bytes = %d, want 128 (usage=%+v)", repoBytes, usage)
	}
	var defaultBytes int64 = -1
	for _, row := range usage.BlobStores {
		if row.BlobStore == "default" {
			defaultBytes = row.Bytes
		}
	}
	if defaultBytes != 128 {
		t.Fatalf("default store bytes = %d, want 128 (usage=%+v)", defaultBytes, usage)
	}
}

func TestStorageUsageEndpointRequiresPrivilege(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/usage", nil, false)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated usage status = %d, want 401", response.StatusCode)
	}
	response.Body.Close()
}
