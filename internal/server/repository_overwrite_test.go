package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestHostedOverwriteAdministrationAndRawRoute(t *testing.T) {
	fixture := newServerFixture(t)
	create := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/repositories", []byte(`{"name":"locked","format":"raw","type":"hosted","allowOverwrite":false}`), "application/json", testToken)
	assertStatus(t, create, http.StatusCreated)
	var created struct {
		AllowOverwrite *bool `json:"allowOverwrite"`
	}
	if err := json.NewDecoder(create.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	create.Body.Close()
	if created.AllowOverwrite == nil || *created.AllowOverwrite {
		t.Fatalf("created policy = %v", created.AllowOverwrite)
	}
	path := "/repository/locked/releases/tool.bin"
	put := func(body string, want int) {
		t.Helper()
		response := fixture.request(t, http.MethodPut, path, []byte(body), true)
		assertStatus(t, response, want)
		response.Body.Close()
	}
	put("first", http.StatusCreated)
	put("first", http.StatusCreated)
	put("second", http.StatusConflict)
	update := fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/locked", []byte(`{"format":"raw","type":"hosted"}`), "application/json", testToken)
	assertStatus(t, update, http.StatusOK)
	update.Body.Close()
	put("second", http.StatusConflict)
	update = fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/locked", []byte(`{"format":"raw","type":"hosted","allowOverwrite":true}`), "application/json", testToken)
	assertStatus(t, update, http.StatusOK)
	update.Body.Close()
	put("second", http.StatusCreated)
	read := fixture.request(t, http.MethodGet, path, nil, true)
	assertStatus(t, read, http.StatusOK)
	contents, err := io.ReadAll(read.Body)
	read.Body.Close()
	if err != nil || string(contents) != "second" {
		t.Fatalf("read content = %q, %v", contents, err)
	}
}

func TestProxyRejectsOverwriteSettingViaAPI(t *testing.T) {
	fixture := newServerFixture(t)
	response := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/repositories", []byte(`{"name":"invalid","format":"raw","type":"proxy","upstream":"https://example.test","allowOverwrite":true}`), "application/json", testToken)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}
