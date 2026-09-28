package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/httpx"
)

func TestAccountCreationValidationAndAddressableNames(t *testing.T) {
	fixture := newServerFixture(t)
	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{"missing username", `{"password":"secret"}`, "invalid_username"},
		{"missing password", `{"username":"operator"}`, "password_required"},
		{"path separator", `{"username":"ops/team","password":"secret"}`, "invalid_username"},
		{"basic separator", `{"username":"ops:team","password":"secret"}`, "invalid_username"},
		{"dot segment", `{"username":"..","password":"secret"}`, "invalid_username"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users", []byte(test.body), "application/json", true)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			var problem httpx.ProblemDetails
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != test.code {
				t.Fatalf("code = %q, want %q", problem.Code, test.code)
			}
		})
	}

	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/users", []byte(`{"username":"Release.Bot@example.com","password":"secret"}`), "application/json", true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", response.StatusCode)
	}
	location := response.Header.Get("Location")
	if !strings.HasSuffix(location, "/Release.Bot@example.com") {
		t.Fatalf("Location = %q", location)
	}
	item := fixture.request(t, http.MethodGet, location, nil, true)
	defer item.Body.Close()
	if item.StatusCode != http.StatusOK {
		t.Fatalf("GET Location status = %d, want 200", item.StatusCode)
	}
}

func TestReservedRepositoryNameIsClientError(t *testing.T) {
	fixture := newServerFixture(t)
	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/repositories", []byte(`{"name":"default","format":"raw","type":"hosted"}`), "application/json", true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "reserved_repository_name" {
		t.Fatalf("code = %q, want reserved_repository_name", problem.Code)
	}
}
