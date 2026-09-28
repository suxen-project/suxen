package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// TestDeleteRoleReferencedByProviderReturns409 asserts the served role DELETE
// contract: a role still named by an OIDC provider mapping is rejected with 409
// and the role_in_use_by_provider problem code.
func TestDeleteRoleReferencedByProviderReturns409(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()

	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name:       "reader",
		Privileges: []string{"repository:raw:read"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateOIDCProvider(ctx, domain.OIDCProvider{
		Name:         "corp",
		Issuer:       "https://issuer.example.com",
		ClientID:     "client",
		DefaultRoles: []string{"reader"},
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(t, http.MethodDelete, "/api/v1/roles/reader", nil, true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusConflict)
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "role_in_use_by_provider" {
		t.Fatalf("problem code = %q, want role_in_use_by_provider", problem.Code)
	}
}
