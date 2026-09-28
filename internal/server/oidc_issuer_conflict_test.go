package server

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestOIDCProviderIssuerConflictOnUpdateReturnsConflict(t *testing.T) {
	fixture := newServerFixture(t)
	for _, provider := range []struct{ name, issuer string }{
		{"first", "https://first.example.test"},
		{"second", "https://second.example.test"},
	} {
		body := []byte(`{"name":"` + provider.name + `","issuer":"` + provider.issuer + `","clientId":"suxen"}`)
		response := fixture.requestWithContentType(t, http.MethodPost,
			"/api/v1/oidc-providers", body, "application/json", true)
		if response.StatusCode != http.StatusCreated {
			content, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("create %s = %d: %s", provider.name, response.StatusCode, content)
		}
		response.Body.Close()
	}
	update := []byte(`{"issuer":"https://first.example.test","clientId":"suxen"}`)
	response := fixture.requestWithContentType(t, http.MethodPut,
		"/api/v1/oidc-providers/second", update, "application/json", true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("duplicate issuer update = %d, want 409: %s", response.StatusCode, content)
	}
	stored, err := fixture.Metadata.OIDCProvider(context.Background(), "second")
	if err != nil || stored.Issuer != "https://second.example.test" {
		t.Fatalf("conflicting update changed second provider: issuer=%q, err=%v", stored.Issuer, err)
	}
}
