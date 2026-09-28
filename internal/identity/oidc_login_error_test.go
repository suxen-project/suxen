package identity

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/httpx"
)

func TestInvalidOIDCTokenHidesVerifierCause(t *testing.T) {
	const secret = "token-secret-must-not-leak"
	var logs bytes.Buffer
	service := &Service{Log: slog.New(slog.NewTextHandler(&logs, nil))}
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/provider/callback", nil)
	request = httpx.WithRequestLog(request, &httpx.RequestLog{RequestID: "request-123"})
	response := httptest.NewRecorder()
	service.rejectInvalidOIDCToken(response, request, "provider", errors.New(secret))
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	for _, expected := range []string{secret, "provider", "request-123"} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("log missing %q: %s", expected, logs.String())
		}
	}
}
