package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type deletingRoleAfterReadStore struct {
	store.Store
	deleteRole func() error
	once       sync.Once
}

func (s *deletingRoleAfterReadStore) Role(ctx context.Context, name string) (domain.Role, error) {
	role, err := s.Store.Role(ctx, name)
	if err == nil && name == "reader" {
		s.once.Do(func() { err = s.deleteRole() })
	}
	return role, err
}

func TestUserCreationReportsRoleRemovedAfterValidation(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{Name: "reader", Privileges: []string{"repository:raw:read"}}); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.metadata = &deletingRoleAfterReadStore{
		Store: fixture.Metadata,
		deleteRole: func() error {
			return fixture.Metadata.DeleteRole(ctx, "reader", store.Ownership{})
		},
	}
	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users",
		[]byte(`{"username":"race-reader","password":"password","roles":["reader"]}`),
		"application/json", true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("role removed after validation = %d, want 404", response.StatusCode)
	}
	if _, err := fixture.Metadata.User(ctx, "race-reader"); err != domain.ErrNotFound {
		t.Fatalf("failed role assignment persisted user: %v", err)
	}
}
