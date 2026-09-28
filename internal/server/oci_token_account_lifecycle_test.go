package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/store"
)

const ociUploadPath = "/v2/acme/app/blobs/uploads/"

func issueOCIWithPassword(t *testing.T, fixture *serverFixture, username, password string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:acme/app:pull,push", nil)
	req.SetBasicAuth(username, password)
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	assertStatus(t, response, http.StatusOK)
	return decodeOCIAccessToken(t, response)
}

func assertOCIUploadStatus(t *testing.T, fixture *serverFixture, token string, want int) {
	t.Helper()
	response := fixture.requestWithBearer(t, http.MethodPost, ociUploadPath, nil, "", token)
	defer response.Body.Close()
	assertStatus(t, response, want)
}

func createOCIUserViaAPI(t *testing.T, fixture *serverFixture, name, password string, admin bool, roles []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"username": name, "password": password, "admin": admin, "roles": roles,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users", body, "application/json", true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusCreated)
}

func TestOCIBearerRejectsRecreatedAPIAccount(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name        string
		admin       bool
		roles       []string
		replacement string
	}{
		{"administrator/different password", true, []string{}, "replacement-password"},
		{"administrator/same password", true, []string{}, "original-password"},
		{"role user/different password", false, []string{"oci-publisher"}, "replacement-password"},
		{"role user/same password", false, []string{"oci-publisher"}, "original-password"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			fixture := newServerFixture(t)
			if !scenario.admin {
				role := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/roles", []byte(`{"name":"oci-publisher","privileges":["repository:oci:read","repository:oci:write"]}`), "application/json", true)
				assertStatus(t, role, http.StatusCreated)
				role.Body.Close()
			}
			const username = "release-operator"
			createOCIUserViaAPI(t, fixture, username, "original-password", scenario.admin, scenario.roles)
			oldToken := issueOCIWithPassword(t, fixture, username, "original-password")
			assertOCIUploadStatus(t, fixture, oldToken, http.StatusAccepted)

			deleted := fixture.request(t, http.MethodDelete, "/api/v1/users/"+username, nil, true)
			assertStatus(t, deleted, http.StatusNoContent)
			deleted.Body.Close()
			assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)

			createOCIUserViaAPI(t, fixture, username, scenario.replacement, scenario.admin, scenario.roles)
			assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)
			newToken := issueOCIWithPassword(t, fixture, username, scenario.replacement)
			assertOCIUploadStatus(t, fixture, newToken, http.StatusAccepted)
		})
	}
}

func TestOCIBearerMintedFromAPITokenRejectsRecreatedOwner(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const username = "api-publisher"
	createOCIUserViaAPI(t, fixture, username, "same-password", true, []string{})
	created := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users/"+username+"/tokens", []byte(`{"name":"publisher","scopes":["repository:oci:read","repository:oci:write"]}`), "application/json", true)
	assertStatus(t, created, http.StatusCreated)
	var apiToken struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(created.Body).Decode(&apiToken); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if apiToken.Token == "" {
		t.Fatal("API token response had no token")
	}
	issued := fixture.requestWithBearer(t, http.MethodGet, "/v2/token?scope=repository:acme/app:pull,push", nil, "", apiToken.Token)
	assertStatus(t, issued, http.StatusOK)
	oldToken := decodeOCIAccessToken(t, issued)
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusAccepted)

	deleted := fixture.request(t, http.MethodDelete, "/api/v1/users/"+username, nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	createOCIUserViaAPI(t, fixture, username, "same-password", true, []string{})
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)
	assertOCIUploadStatus(t, fixture, issueOCIWithPassword(t, fixture, username, "same-password"), http.StatusAccepted)
}

func TestOCILegacyLocalBearerFailsClosed(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	createOCIUserViaAPI(t, fixture, "legacy-admin", "password", true, []string{})
	legacy, err := identity.SignAccessToken([]byte(fixture.Handler.cfg.OIDCStateSecret), identity.AccessToken{
		Repository: "oci", Subject: "legacy-admin", Local: true, Admin: true,
		Actions: []string{"pull", "push"}, ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOCIUploadStatus(t, fixture, legacy, http.StatusUnauthorized)
	assertOCIUploadStatus(t, fixture, issueOCIWithPassword(t, fixture, "legacy-admin", "password"), http.StatusAccepted)
}

func TestOCIBearerRejectsPrunedAndReprovisionedAccount(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	provision := func(body string, prune bool) {
		t.Helper()
		path := "/api/v1/provision"
		if prune {
			path += "?prune=true"
		}
		response := fixture.requestWithContentType(t, http.MethodPost, path, []byte(body), "application/json", true)
		defer response.Body.Close()
		assertStatus(t, response, http.StatusOK)
		var report struct {
			Results []struct {
				Status string `json:"status"`
			} `json:"results"`
		}
		if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
			t.Fatal(err)
		}
		for _, result := range report.Results {
			if result.Status == "failed" {
				t.Fatalf("provisioning failed: %+v", report)
			}
		}
	}
	const account = `{"apiVersion":"suxen.io/v1","resources":[{"kind":"user","name":"managed-publisher","spec":{"password":"same-password","admin":true,"roles":[]}}]}`
	const empty = `{"apiVersion":"suxen.io/v1","resources":[]}`
	provision(account, false)
	oldToken := issueOCIWithPassword(t, fixture, "managed-publisher", "same-password")
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusAccepted)
	provision(empty, true)
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)
	provision(account, false)
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)
	assertOCIUploadStatus(t, fixture, issueOCIWithPassword(t, fixture, "managed-publisher", "same-password"), http.StatusAccepted)
}

type replacingAccountBeforeAuthorizationStore struct {
	store.Store
	name string
	once sync.Once
	err  error
}

func (s *replacingAccountBeforeAuthorizationStore) LocalAuthorization(ctx context.Context, username, accountIdentity string) (domain.User, []string, error) {
	if username == s.name {
		s.once.Do(func() {
			s.err = s.Store.DeleteUser(ctx, username, store.Ownership{})
			if s.err == nil {
				s.err = s.Store.SaveUser(ctx, store.UserSave{Username: username, Password: "same-password", Admin: true, Roles: []string{}, Create: true})
			}
		})
		if s.err != nil {
			return domain.User{}, nil, s.err
		}
	}
	return s.Store.LocalAuthorization(ctx, username, accountIdentity)
}

func TestOCITokenMintRejectsAccountReplacedAfterAuthentication(t *testing.T) {
	fixture := newServerFixture(t)
	const username = "mint-race"
	createOCIUserViaAPI(t, fixture, username, "same-password", true, []string{})
	wrapped := &replacingAccountBeforeAuthorizationStore{Store: fixture.Metadata, name: username}
	fixture.Handler.identity.SetMetadata(wrapped)
	req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:acme/app:pull,push", nil)
	req.SetBasicAuth(username, "same-password")
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer response.Body.Close()
	assertStatus(t, response, http.StatusUnauthorized)
	if wrapped.err != nil {
		t.Fatal(wrapped.err)
	}
	assertOCIUploadStatus(t, fixture, issueOCIWithPassword(t, fixture, username, "same-password"), http.StatusAccepted)
}

func TestOCIBearerRejectsAccountReplacedAfterIdentityLookup(t *testing.T) {
	fixture := newServerFixture(t)
	const username = "upload-race"
	createOCIUserViaAPI(t, fixture, username, "same-password", true, []string{})
	oldToken := issueOCIWithPassword(t, fixture, username, "same-password")
	wrapped := &replacingAccountBeforeAuthorizationStore{Store: fixture.Metadata, name: username}
	fixture.Handler.identity.SetMetadata(wrapped)
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusForbidden)
	if wrapped.err != nil {
		t.Fatal(wrapped.err)
	}
	assertOCIUploadStatus(t, fixture, oldToken, http.StatusUnauthorized)
	assertOCIUploadStatus(t, fixture, issueOCIWithPassword(t, fixture, username, "same-password"), http.StatusAccepted)
}

func TestWhoAmIRejectsAccountReplacedAfterAuthentication(t *testing.T) {
	fixture := newServerFixture(t)
	const username = "whoami-race"
	createOCIUserViaAPI(t, fixture, username, "same-password", true, []string{})
	wrapped := &replacingAccountBeforeAuthorizationStore{Store: fixture.Metadata, name: username}
	fixture.Handler.metadata = wrapped
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.SetBasicAuth(username, "same-password")
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer response.Body.Close()
	assertStatus(t, response, http.StatusUnauthorized)
	if wrapped.err != nil {
		t.Fatal(wrapped.err)
	}
}
