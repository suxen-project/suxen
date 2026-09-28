package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type demotingAccountStore struct {
	store.Store
	once   sync.Once
	demote func()
}

func (s *demotingAccountStore) User(ctx context.Context, name string) (domain.User, error) {
	user, err := s.Store.User(ctx, name)
	if err == nil && name == "operator" {
		s.once.Do(s.demote)
	}
	return user, err
}

func TestPartialUserUpdatePreservesConcurrentDemotion(t *testing.T) {
	for _, update := range []struct {
		name string
		body string
	}{
		{"password", `{"password":"new-password"}`},
		{"roles", `{"roles":[]}`},
	} {
		t.Run(update.name, func(t *testing.T) {
			fixture := newServerFixture(t)
			ctx := context.Background()
			if err := fixture.Metadata.CreateUser(ctx, "operator", "old-password", true); err != nil {
				t.Fatal(err)
			}
			wrapper := &demotingAccountStore{Store: fixture.Metadata}
			wrapper.demote = func() {
				if err := fixture.Metadata.SaveUser(ctx, store.UserSave{Username: "operator", Admin: false}); err != nil {
					t.Fatal(err)
				}
			}
			fixture.Handler.metadata = wrapper
			response := fixture.request(t, http.MethodPut, "/api/v1/users/operator", []byte(update.body), true)
			assertStatus(t, response, http.StatusOK)
			response.Body.Close()
			user, err := fixture.Metadata.User(ctx, "operator")
			if err != nil {
				t.Fatal(err)
			}
			if user.Admin {
				t.Fatal("partial update restored administrator rights after demotion")
			}
		})
	}
}

func TestUserRolesPUTRequiresArrayAndPreservesAssignments(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{Name: "reader", Privileges: []string{"repository:raw:read"}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateUser(ctx, "reader", "password", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "reader", []string{"reader"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"roles":null}`} {
		response := fixture.request(t, http.MethodPut, "/api/v1/users/reader/roles", []byte(body), true)
		assertStatus(t, response, http.StatusBadRequest)
		response.Body.Close()
		roles, err := fixture.Metadata.UserRoles(ctx, "reader")
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 1 || roles[0] != "reader" {
			t.Fatalf("PUT %s changed assigned roles to %v", body, roles)
		}
	}
	response := fixture.request(t, http.MethodPut, "/api/v1/users/reader/roles", []byte(`{"roles":[]}`), true)
	assertStatus(t, response, http.StatusOK)
	var result struct {
		Roles []string `json:"roles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if result.Roles == nil || len(result.Roles) != 0 {
		t.Fatalf("cleared roles response = %v, want []", result.Roles)
	}
	roles, err := fixture.Metadata.UserRoles(ctx, "reader")
	if err != nil || len(roles) != 0 {
		t.Fatalf("roles after explicit clear = %v, %v", roles, err)
	}
}
