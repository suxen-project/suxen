//go:build noui

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUIFreeBuildRoutesReturnNotFound(t *testing.T) {
	t.Parallel()
	if uiEnabled {
		t.Fatal("noui server build unexpectedly contains the administration UI")
	}
	fixture := newServerFixture(t)

	for _, requestPath := range []string{"/", "/ui", "/ui/", "/ui/app.js"} {
		response := fixture.request(t, http.MethodGet, requestPath, nil, false)
		assertStatus(t, response, http.StatusNotFound)
		response.Body.Close()
	}

	health := fixture.request(t, http.MethodGet, "/healthz", nil, false)
	assertStatus(t, health, http.StatusOK)
	health.Body.Close()
}

func TestUIFreeHandlerReturnsNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ui/index.html", nil)
	fixture.Handler.handleUI(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled UI handler status = %d, want 404", recorder.Code)
	}
}
