package server

import (
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/identity"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCookieAuthenticatedMutationsRequireSameOrigin(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{
			name:       "missing origin",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross origin",
			origin:     "https://attacker.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "same origin reaches authentication",
			origin:     "https://registry.example",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodDelete,
				"https://registry.example/api/v1/webhooks/example",
				nil,
			)
			request.AddCookie(&http.Cookie{
				Name:  identity.SessionCookieName,
				Value: "invalid-session",
			})
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body)
			}
			if test.wantStatus == http.StatusForbidden &&
				!strings.Contains(response.Body.String(), "csrf_validation_failed") {
				t.Fatalf("response omitted CSRF problem code: %s", response.Body)
			}
		})
	}
}

func TestPublicURLDefinesCookieMutationOrigin(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.PublicURL = "https://artifacts.example.com" })

	for _, test := range []struct {
		origin     string
		wantStatus int
	}{
		{origin: "https://artifacts.example.com", wantStatus: http.StatusNoContent},
		{origin: "http://suxen.internal:8080", wantStatus: http.StatusForbidden},
	} {
		request := httptest.NewRequest(
			http.MethodPost,
			"http://suxen.internal:8080/auth/oidc/corporate/logout",
			nil,
		)
		request.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "invalid-session"})
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus {
			t.Fatalf("origin %q status = %d, want %d", test.origin, response.Code, test.wantStatus)
		}
	}
}

func TestBearerMutationIsNotSubjectToCookieCSRFValidation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	request := httptest.NewRequest(
		http.MethodPut,
		"http://registry.example/repository/raw/csrf/bearer.txt",
		strings.NewReader("bearer upload"),
	)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "unrelated-session"})
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		body, _ := io.ReadAll(response.Result().Body)
		t.Fatalf("status = %d, want 201: %s", response.Code, body)
	}
}

func TestSameOriginComparisonRejectsNonOrigins(t *testing.T) {
	tests := []struct {
		actual   string
		expected string
		want     bool
	}{
		{actual: "https://registry.example", expected: "https://registry.example", want: true},
		{actual: "HTTPS://REGISTRY.EXAMPLE", expected: "https://registry.example", want: true},
		{actual: "http://registry.example", expected: "https://registry.example"},
		{actual: "https://registry.example/path", expected: "https://registry.example"},
		{actual: "null", expected: "https://registry.example"},
		{actual: "", expected: "https://registry.example"},
	}

	for _, test := range tests {
		if got := identity.SameOriginStrings(test.actual, test.expected); got != test.want {
			t.Errorf("identity.SameOriginStrings(%q, %q) = %t, want %t", test.actual, test.expected, got, test.want)
		}
	}
}
