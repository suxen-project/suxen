package server

import (
	"context"
	"encoding/json"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPrivilegeAllows(t *testing.T) {
	tests := []struct {
		name     string
		granted  string
		required string
		allowed  bool
	}{
		{
			name:     "global wildcard",
			granted:  "*",
			required: "admin:users:write",
			allowed:  true,
		},
		{
			name:     "repository wildcard",
			granted:  "repository:*:read",
			required: "repository:releases:read",
			allowed:  true,
		},
		{
			name:     "tail wildcard",
			granted:  "admin:*",
			required: "admin:roles:write",
			allowed:  true,
		},
		{
			name:     "different action",
			granted:  "repository:*:read",
			required: "repository:releases:write",
			allowed:  false,
		},
		{
			name:     "different repository",
			granted:  "repository:staging:write",
			required: "repository:releases:write",
			allowed:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := identity.PrivilegeAllows(test.granted, test.required)
			if actual != test.allowed {
				t.Fatalf(
					"identity.PrivilegeAllows(%q, %q) = %v, want %v",
					test.granted,
					test.required,
					actual,
					test.allowed,
				)
			}
		})
	}
}

func TestAnnotationPrivilegeCannotChangeRepositoryPolicy(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, role := range []domain.Role{
		{Name: "asset-scanner", Privileges: []string{"repository:raw:annotate"}},
		{Name: "policy-manager", Privileges: []string{"repository:raw:manage"}},
	} {
		if err := fixture.Metadata.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.Metadata.CreateUser(ctx, "scanner", "scanner-password-12", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "scanner", []string{"asset-scanner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.CreateToken(ctx, "scanner", "scanner-token", "scanner-policy-test", nil); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/repositories/raw/classification"
	body := []byte(`{"rules":[]}`)
	denied := fixture.requestWithBearer(t, http.MethodPut, path, body, "application/json", "scanner-policy-test")
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("annotate-only policy write = %d, want 403", denied.StatusCode)
	}
	denied.Body.Close()
	if err := fixture.Metadata.SetUserRoles(ctx, "scanner", []string{"policy-manager"}); err != nil {
		t.Fatal(err)
	}
	allowed := fixture.requestWithBearer(t, http.MethodPut, path, body, "application/json", "scanner-policy-test")
	if allowed.StatusCode != http.StatusOK && allowed.StatusCode != http.StatusCreated {
		t.Fatalf("manage policy write = %d, want success", allowed.StatusCode)
	}
	allowed.Body.Close()
}

func TestPrivilegeCatalogUsesCanonicalAdminResources(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/privileges", nil)
	(&Server{}).handlePrivileges(recorder, request)

	var catalog struct {
		AdminResources    []string `json:"adminResources"`
		RepositoryActions []string `json:"repositoryActions"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"blob-stores", "oidc-providers", "gc", "verify"} {
		if !slices.Contains(catalog.AdminResources, resource) {
			t.Errorf("admin resources = %v, want %s", catalog.AdminResources, resource)
		}
	}
	if !slices.Contains(catalog.RepositoryActions, "manage") {
		t.Errorf("repository actions = %v, want manage", catalog.RepositoryActions)
	}
}

func TestControlPlanePrivilegeRequirements(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "whoami", publicPrivilegeRequirement},
		{http.MethodGet, "browse", visibleRepositoryPrivilegeRequirement},
		{http.MethodGet, "search", visibleRepositoryPrivilegeRequirement},
		{http.MethodGet, "repositories/releases", "repository:releases:read"},
		{http.MethodGet, "repositories", visibleRepositoryPrivilegeRequirement},
		{http.MethodGet, "repositories/releases/assets", "repository:releases:read"},
		{http.MethodDelete, "repositories/releases/assets/42", "repository:releases:delete"},
		{http.MethodPut, "repositories/releases/assets/42/attributes/scanner", "repository:releases:annotate"},
		{http.MethodPost, "repositories/releases/assets/42/verification", "repository:releases:annotate"},
		{http.MethodPut, "repositories/releases/classification", "repository:releases:manage"},
		{http.MethodGet, "repositories/releases/download-gate", "repository:releases:read"},
		{http.MethodPut, "repositories/releases/download-gate", "repository:releases:manage"},
		{http.MethodDelete, "repositories/releases/trust-policy", "repository:releases:manage"},
		{http.MethodPost, "repositories/releases/cleanup", "repository:releases:delete"},
		{http.MethodGet, "blob-stores", "admin:blob-stores:read"},
		{http.MethodPost, "oidc-providers", "admin:oidc-providers:write"},
		{http.MethodPost, "gc", "admin:gc:run"},
		{http.MethodPost, "verify", "admin:verify:run"},
		{http.MethodGet, "unknown", "admin:access:read"},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			got := controlPlanePrivilegeRequirement(httpx.SplitPath(test.path), test.method)
			if got != test.want {
				t.Fatalf("privilege requirement = %q, want %q", got, test.want)
			}
		})
	}
}
