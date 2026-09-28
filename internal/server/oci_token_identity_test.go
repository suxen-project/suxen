package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/store"
)

func TestOCITokenAnonymousNamedAdministratorRevocation(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"delete", "demote"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			fixture := newServerFixture(t)
			ctx := context.Background()
			if err := fixture.Metadata.CreateUser(ctx, "anonymous", "local-password", true); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:acme/app:pull,push", nil)
			request.SetBasicAuth("anonymous", "local-password")
			recorder := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			assertStatus(t, response, http.StatusOK)
			token := decodeOCIAccessToken(t, response)
			ping := fixture.requestWithBearer(t, http.MethodGet, "/v2/", nil, "", token)
			assertStatus(t, ping, http.StatusOK)
			ping.Body.Close()
			var err error
			if mutation == "delete" {
				err = fixture.Metadata.DeleteUser(ctx, "anonymous", store.Ownership{})
			} else {
				err = fixture.Metadata.UpdateUser(ctx, "anonymous", "", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			upload := fixture.requestWithBearer(t, http.MethodPost, "/v2/acme/app/blobs/uploads/", nil, "", token)
			want := http.StatusForbidden
			if mutation == "delete" {
				want = http.StatusUnauthorized
			}
			assertStatus(t, upload, want)
			upload.Body.Close()
		})
	}
}

func TestOCIAnonymousTokenCannotInheritLocalAccountPrivileges(t *testing.T) {
	t.Parallel()
	fixture := newServerFixtureWithAnonymousRead(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateUser(ctx, "anonymous", "local-password", true); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{Name: "local-reader", Privileges: []string{"repository:oci:read"}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "anonymous", []string{"local-reader"}); err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodGet, "/v2/token", nil, false)
	assertStatus(t, response, http.StatusOK)
	token := decodeOCIAccessToken(t, response)
	if err := fixture.Metadata.UpdateRole(ctx, domain.Role{Name: "anonymous", Privileges: []string{}}); err != nil {
		t.Fatal(err)
	}
	legacyToken, err := identity.SignAccessToken([]byte(fixture.Handler.cfg.OIDCStateSecret), identity.AccessToken{
		Repository: "oci", Subject: "anonymous", Actions: []string{"pull", "push"},
		ExpiresAt: time.Now().Add(time.Minute).Unix(), Admin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{token, legacyToken} {
		ping := fixture.requestWithBearer(t, http.MethodGet, "/v2/", nil, "", candidate)
		assertStatus(t, ping, http.StatusForbidden)
		ping.Body.Close()
	}
}
