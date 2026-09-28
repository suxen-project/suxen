package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type deleteGroupMemberBeforeCreateStore struct {
	store.Store
	once          sync.Once
	mutationError error
}

func (metadata *deleteGroupMemberBeforeCreateStore) SaveRepository(ctx context.Context, save store.RepositorySave) error {
	if save.Repository.Name == "group-after-preflight" {
		metadata.once.Do(func() {
			metadata.mutationError = metadata.Store.DeleteRepository(ctx, "leaf-before-preflight", store.Ownership{Force: true})
		})
		if metadata.mutationError != nil {
			return metadata.mutationError
		}
	}
	return metadata.Store.SaveRepository(ctx, save)
}

func TestGroupCreateRejectsMemberDeletedAfterAPIPreflight(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "leaf-before-preflight", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	wrapped := &deleteGroupMemberBeforeCreateStore{Store: fixture.Metadata}
	fixture.Handler.metadata = wrapped
	fixture.Handler.repositories = controlplane.NewRepositoryService(wrapped)
	fixture.Handler.content.SetMetadata(wrapped)
	create := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"group-after-preflight","format":"raw","type":"group","members":["leaf-before-preflight"]}`),
		"application/json", testToken)
	assertStatus(t, create, http.StatusBadRequest)
	create.Body.Close()
	if _, err := fixture.Metadata.Repository(ctx, "group-after-preflight"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("dangling group persisted: %v", err)
	}
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{Name: "leaf-before-preflight", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	valid := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"group-after-preflight","format":"raw","type":"group","members":["leaf-before-preflight"]}`),
		"application/json", testToken)
	assertStatus(t, valid, http.StatusCreated)
	valid.Body.Close()
}
