package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/rawpath"
)

func TestRawPathAndLocationRoundTrip(t *testing.T) {
	fixture := newServerFixture(t)
	assetPath := "release notes/what? #1/100% café.txt"
	requestPath, err := rawpath.URLPath("raw", assetPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("round trip")
	upload := fixture.request(t, http.MethodPut, requestPath, payload, true)
	assertStatus(t, upload, http.StatusCreated)
	location := upload.Header.Get("Location")
	upload.Body.Close()
	if location != requestPath {
		t.Fatalf("Location = %q, want %q", location, requestPath)
	}
	stored, err := fixture.Metadata.Asset(context.Background(), "raw", assetPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Path != assetPath {
		t.Fatalf("stored path = %q", stored.Path)
	}
	listed := fixture.request(t, http.MethodGet,
		"/api/v1/repositories/raw/assets?prefix="+url.QueryEscape("release notes/"), nil, true)
	assertStatus(t, listed, http.StatusOK)
	var page httpx.CollectionPage[domain.Asset]
	if err := json.NewDecoder(listed.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Path != assetPath {
		t.Fatalf("listed assets = %+v", page.Items)
	}
	download := fixture.request(t, http.MethodGet, location, nil, true)
	assertStatus(t, download, http.StatusOK)
	assertBody(t, download, payload)
	deleted := fixture.request(t, http.MethodDelete, location, nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
}

func TestRawPathRejectsAmbiguousSegments(t *testing.T) {
	fixture := newServerFixture(t)
	for _, requestPath := range []string{
		"/repository/raw//a", "/repository/raw/a/", "/repository/raw/a//b",
		"/repository/raw/a/./b", "/repository/raw/a/../b", "/repository/raw/a/%2e%2e/b",
		"/repository/raw/a/%1fb", "/repository/raw/a/%7fb", "/repository/raw/a/%5cb",
	} {
		response := fixture.request(t, http.MethodPut, requestPath, []byte("invalid"), true)
		assertStatus(t, response, http.StatusBadRequest)
		response.Body.Close()
	}
}

func TestRawPathRejectsOverlongPathBeforeUpload(t *testing.T) {
	fixture := newServerFixture(t)
	path := "/repository/raw/" + strings.Repeat("a", domain.MaxAssetPathBytes+1)
	response := fixture.request(t, http.MethodPut, path, []byte("invalid"), true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusBadRequest)
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "invalid_path" {
		t.Fatalf("problem code = %q, want invalid_path", problem.Code)
	}
}

func TestRawPathRejectsInvalidUTF8BeforeUpload(t *testing.T) {
	fixture := newServerFixture(t)
	for _, requestPath := range []string{"/repository/raw/bad%FF", "/repository/raw/bad%C3"} {
		response := fixture.request(t, http.MethodPut, requestPath, []byte("invalid"), true)
		assertStatus(t, response, http.StatusBadRequest)
		var problem httpx.ProblemDetails
		if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if problem.Code != "invalid_path" {
			t.Fatalf("%s: problem code = %q", requestPath, problem.Code)
		}
	}
}
