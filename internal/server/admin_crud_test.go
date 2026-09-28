package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestUserItemCRUD(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/users",
		[]byte(`{
			"username":"ui-operator",
			"password":"initial-password",
			"admin":false,
			"roles":[]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	if location := created.Header.Get("Location"); location != "/api/v1/users/ui-operator" {
		t.Fatalf("created user Location = %q", location)
	}
	var createdUser userResponse
	if err := json.NewDecoder(created.Body).Decode(&createdUser); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if createdUser.Username != "ui-operator" || createdUser.Admin || len(createdUser.Roles) != 0 {
		t.Fatalf("unexpected created user response: %+v", createdUser)
	}

	createdToken := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/users/ui-operator/tokens",
		[]byte(`{"name":"automation","scopes":["repository:raw:read"]}`),
		"application/json",
		testToken,
	)
	assertStatus(t, createdToken, http.StatusCreated)
	var token tokenCreatedResponse
	if err := json.NewDecoder(createdToken.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	createdToken.Body.Close()
	wantTokenLocation := fmt.Sprintf("/api/v1/users/ui-operator/tokens/%d", token.ID)
	if location := createdToken.Header.Get("Location"); location != wantTokenLocation {
		t.Fatalf("created token Location = %q, want %q", location, wantTokenLocation)
	}
	if token.Token == "" || token.Username != "ui-operator" || token.Name != "automation" {
		t.Fatalf("unexpected created token response: %+v", token)
	}

	read := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/users/ui-operator",
		nil,
		true,
	)
	assertStatus(t, read, http.StatusOK)
	var user struct {
		Username string   `json:"username"`
		Admin    bool     `json:"admin"`
		Roles    []string `json:"roles"`
	}
	if err := json.NewDecoder(read.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if user.Username != "ui-operator" || user.Admin || len(user.Roles) != 0 {
		t.Fatalf("unexpected created user: %+v", user)
	}

	updated := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/users/ui-operator",
		[]byte(`{
			"password":"replacement-password",
			"admin":true,
			"roles":["administrator"]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, updated, http.StatusOK)
	updated.Body.Close()
	if _, authenticated := fixture.Metadata.AuthenticatePassword(
		context.Background(),
		"ui-operator",
		"replacement-password",
	); !authenticated {
		t.Fatal("updated password did not authenticate")
	}
	roles, err := fixture.Metadata.UserRoles(context.Background(), "ui-operator")
	if err != nil || len(roles) != 1 || roles[0] != "administrator" {
		t.Fatalf("updated user roles: roles=%v err=%v", roles, err)
	}

	deleted := fixture.request(
		t,
		http.MethodDelete,
		"/api/v1/users/ui-operator",
		nil,
		true,
	)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	if _, err := fixture.Metadata.User(context.Background(), "ui-operator"); !errors.Is(
		err,
		domain.ErrNotFound,
	) {
		t.Fatalf("deleted user returned %v, want ErrNotFound", err)
	}
}

func TestPutUpsertsNamedControlPlaneResources(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	tests := []struct {
		name        string
		path        string
		body        string
		identityKey string
	}{
		{
			name:        "repository",
			path:        "/api/v1/repositories/put-created",
			body:        `{"format":"raw","type":"hosted"}`,
			identityKey: "name",
		},
		{
			name:        "role",
			path:        "/api/v1/roles/put-created",
			body:        `{"description":"created with PUT","privileges":["repository:raw:read"]}`,
			identityKey: "name",
		},
		{
			name:        "user",
			path:        "/api/v1/users/put-created",
			body:        `{"password":"created-with-put","roles":[]}`,
			identityKey: "username",
		},
		{
			name:        "OIDC provider",
			path:        "/api/v1/oidc-providers/put-created",
			body:        `{"issuer":"https://identity.example/realms/put","clientId":"suxen"}`,
			identityKey: "name",
		},
		{
			name:        "cleanup policy",
			path:        "/api/v1/cleanup-policies/put-created",
			body:        `{"repositories":["raw"],"criteria":[{"path":"sys.path","op":"exists"}]}`,
			identityKey: "name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created := fixture.requestWithBearer(
				t,
				http.MethodPut,
				test.path,
				[]byte(test.body),
				"application/json",
				testToken,
			)
			assertStatus(t, created, http.StatusCreated)
			if location := created.Header.Get("Location"); location != test.path {
				t.Fatalf("Location = %q, want %q", location, test.path)
			}
			var representation map[string]any
			if err := json.NewDecoder(created.Body).Decode(&representation); err != nil {
				t.Fatal(err)
			}
			created.Body.Close()
			if representation[test.identityKey] != "put-created" {
				t.Fatalf("created representation = %+v", representation)
			}

			updated := fixture.requestWithBearer(
				t,
				http.MethodPut,
				test.path,
				[]byte(test.body),
				"application/json",
				testToken,
			)
			assertStatus(t, updated, http.StatusOK)
			updated.Body.Close()
		})
	}
}

func TestAssetAndAttributeCRUD(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	uploaded := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/ui/release.bin",
		[]byte("UI managed artifact"),
		true,
	)
	assertStatus(t, uploaded, http.StatusCreated)
	uploaded.Body.Close()

	asset, err := fixture.Metadata.Asset(context.Background(), "raw", "ui/release.bin")
	if err != nil {
		t.Fatal(err)
	}
	assetPath := fmt.Sprintf("/api/v1/repositories/raw/assets/%d", asset.ID)
	read := fixture.request(t, http.MethodGet, assetPath, nil, true)
	assertStatus(t, read, http.StatusOK)
	read.Body.Close()

	attributePath := assetPath + "/attributes/scan"
	setAttribute := fixture.requestWithBearer(
		t,
		http.MethodPut,
		attributePath,
		[]byte(`{"status":"passed"}`),
		"application/json",
		testToken,
	)
	assertStatus(t, setAttribute, http.StatusCreated)
	setAttribute.Body.Close()
	readAttribute := fixture.request(t, http.MethodGet, attributePath, nil, true)
	assertStatus(t, readAttribute, http.StatusOK)
	var attribute map[string]any
	if err := json.NewDecoder(readAttribute.Body).Decode(&attribute); err != nil {
		t.Fatal(err)
	}
	readAttribute.Body.Close()
	if attribute["status"] != "passed" {
		t.Fatalf("unexpected attribute: %+v", attribute)
	}

	deletedAttribute := fixture.request(t, http.MethodDelete, attributePath, nil, true)
	assertStatus(t, deletedAttribute, http.StatusNoContent)
	deletedAttribute.Body.Close()
	readAttribute = fixture.request(t, http.MethodGet, attributePath, nil, true)
	assertStatus(t, readAttribute, http.StatusNotFound)
	readAttribute.Body.Close()

	deletedAsset := fixture.request(t, http.MethodDelete, assetPath, nil, true)
	assertStatus(t, deletedAsset, http.StatusNoContent)
	deletedAsset.Body.Close()
	if _, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"ui/release.bin",
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted asset returned %v, want ErrNotFound", err)
	}
}
