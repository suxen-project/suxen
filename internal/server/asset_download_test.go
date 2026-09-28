package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	_ "github.com/suxen-project/suxen/plugins/format/pypi"
)

func TestAssetIDDownloadServesQueryPartitionedPyPICache(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "pypi-proxy", Format: "pypi", Type: "proxy", Upstream: "https://mirror.example"},
		{Name: "pypi-group", Format: "pypi", Type: "group", Members: []string{"pypi-proxy"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	const publicPath = "files/https/cdn.example/widget-1.0.whl"
	const signedQuery = "token=private-signature"
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status, body, contentType := http.StatusNotFound, "", "text/plain"
		switch request.URL.String() {
		case "https://mirror.example/simple/widget":
			status, body, contentType = http.StatusOK,
				`<a href="https://cdn.example/widget-1.0.whl?`+signedQuery+`">wheel</a>`, "text/html"
		case "https://cdn.example/widget-1.0.whl?" + signedQuery:
			status, body, contentType = http.StatusOK, "signed wheel", "application/octet-stream"
		}
		return &http.Response{
			StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})})
	for _, path := range []string{
		"/repository/pypi-group/simple/widget/",
		"/repository/pypi-group/" + publicPath + "?" + signedQuery,
	} {
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		response.Body.Close()
	}
	response := fixture.request(t, http.MethodGet, "/api/v1/repositories/pypi-group/assets?prefix=files/", nil, true)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[domain.Asset]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 1 || page.Items[0].FormatPath != publicPath || page.Items[0].Path == publicPath {
		t.Fatalf("cached wheel listing = %+v", page.Items)
	}
	if strings.Contains(page.Items[0].Path, signedQuery) {
		t.Fatalf("signed URL leaked in cache path %q", page.Items[0].Path)
	}
	if err := fixture.Metadata.UpdateRole(ctx, domain.Role{
		Name: "anonymous", Privileges: []string{"repository:pypi-group:read"},
	}); err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("/api/v1/repositories/pypi-group/assets/%d/download", page.Items[0].ID)
	response = fixture.request(t, http.MethodGet, endpoint, nil, false)
	assertStatus(t, response, http.StatusOK)
	assertBody(t, response, []byte("signed wheel"))
	if disposition := response.Header.Get("Content-Disposition"); disposition != "attachment" {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	response = fixture.request(t, http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/pypi-proxy/assets/%d/download", page.Items[0].ID), nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()
}

func TestGroupAssetIDDownloadHonorsVisibilityAndMemberTrust(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "raw-second", Format: "raw", Type: "hosted"},
		{Name: "raw-group", Format: "raw", Type: "group", Members: []string{"raw", "raw-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	firstUpload := fixture.request(t, http.MethodPut, "/repository/raw/shared.bin", []byte("first"), true)
	assertStatus(t, firstUpload, http.StatusCreated)
	firstUpload.Body.Close()
	first, err := fixture.Metadata.Asset(ctx, "raw", "shared.bin")
	if err != nil {
		t.Fatal(err)
	}
	shadowed, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "raw-second", Path: "shared.bin", Digest: "sha256:shadowed",
	})
	if err != nil {
		t.Fatal(err)
	}
	visibleURL := fmt.Sprintf("/api/v1/repositories/raw-group/assets/%d/download", first.ID)
	response := fixture.request(t, http.MethodGet, visibleURL, nil, true)
	assertStatus(t, response, http.StatusOK)
	assertBody(t, response, []byte("first"))
	response = fixture.request(t, http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/raw-group/assets/%d/download", shadowed.ID), nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
	if err := fixture.Metadata.SetDownloadGate(ctx, domain.DownloadGate{
		Repository: "raw", Enabled: true,
		Criteria: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}},
	}); err != nil {
		t.Fatal(err)
	}
	response = fixture.request(t, http.MethodGet, visibleURL, nil, true)
	assertStatus(t, response, http.StatusForbidden)
	response.Body.Close()
	if err := fixture.Metadata.SetAttributes(ctx, "raw", first.ID, "scan", map[string]any{
		"status": "passed",
	}); err != nil {
		t.Fatal(err)
	}
	response = fixture.request(t, http.MethodGet, visibleURL, nil, true)
	assertStatus(t, response, http.StatusOK)
	assertBody(t, response, []byte("first"))
	_, publicKey := provenanceTestKey(t)
	if err := fixture.Metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: "raw", Mode: "verify-on-pull", PublicKeys: []string{publicKey},
	}); err != nil {
		t.Fatal(err)
	}
	response = fixture.request(t, http.MethodGet, visibleURL, nil, true)
	assertStatus(t, response, http.StatusForbidden)
	response.Body.Close()
}
