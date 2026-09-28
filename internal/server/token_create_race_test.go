package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type deletingUserAfterTokenReadStore struct {
	store.Store
	once sync.Once
	err  error
}

func (s *deletingUserAfterTokenReadStore) User(ctx context.Context, name string) (domain.User, error) {
	user, err := s.Store.User(ctx, name)
	if err == nil && name == "token-owner" {
		s.once.Do(func() {
			s.err = s.Store.DeleteUser(ctx, name, store.Ownership{})
		})
		if s.err != nil {
			return domain.User{}, s.err
		}
	}
	return user, err
}

func TestTokenCreationReportsUserRemovedAfterValidation(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateUser(ctx, "token-owner", "owner-password", false); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.metadata = &deletingUserAfterTokenReadStore{Store: fixture.Metadata}
	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users/token-owner/tokens",
		[]byte(`{"name":"automation"}`), "application/json", true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("user removed before token insert = %d, want 404", response.StatusCode)
	}
}
