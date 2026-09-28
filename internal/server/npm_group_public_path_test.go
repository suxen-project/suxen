package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestNpmGroupShadowsOpaqueProxyKeysByPublicPath(t *testing.T) {
	fixture := newServerFixture(t)
	for _, repository := range []domain.Repository{
		{Name: "npm-first", Format: "npm", Type: "proxy", Upstream: "https://first.example"},
		{Name: "npm-second", Format: "npm", Type: "proxy", Upstream: "https://second.example"},
		{Name: "npm-group", Format: "npm", Type: "group", Members: []string{"npm-first", "npm-second"}},
		{Name: "npm-hosted", Format: "npm", Type: "hosted"},
		{Name: "npm-hosted-group", Format: "npm", Type: "group", Members: []string{"npm-hosted", "npm-second"}},
	} {
		createTestRepository(t, fixture, repository)
	}
	archive := proxyNpmArchive(t, "1.0.0")
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/pkg" {
			packument := fmt.Sprintf(`{"versions":{"1.0.0":{"dist":{"tarball":"https://%s/pkg/-/pkg-1.0.0.tgz"}}}}`, request.URL.Host)
			return testHTTPResponse(request, http.StatusOK, packument), nil
		}
		return testHTTPResponse(request, http.StatusOK, string(archive)), nil
	})})
	for _, repository := range []string{"npm-first", "npm-second"} {
		for _, path := range []string{"pkg", "pkg/-/pkg-1.0.0.tgz"} {
			response := fixture.request(t, http.MethodGet, "/repository/"+repository+"/"+path, nil, true)
			assertStatus(t, response, http.StatusOK)
			response.Body.Close()
		}
	}
	readAssets := func(path string) httpx.CollectionPage[domain.Asset] {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, http.StatusOK)
		defer response.Body.Close()
		var page httpx.CollectionPage[domain.Asset]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := readAssets("/api/v1/repositories/npm-group/assets?prefix=pkg/-/")
	if len(page.Items) != 1 || page.Items[0].Repository != "npm-group" ||
		page.Items[0].FormatPath != "pkg/-/pkg-1.0.0.tgz" {
		t.Fatalf("group assets = %+v", page.Items)
	}
	visibleID := page.Items[0].ID
	secondPage := readAssets("/api/v1/repositories/npm-second/assets?prefix=pkg/-/")
	if len(secondPage.Items) != 1 || secondPage.Items[0].Path == page.Items[0].Path {
		t.Fatalf("proxy cache identities were not distinct: first=%+v second=%+v", page.Items, secondPage.Items)
	}
	shadowedID := secondPage.Items[0].ID
	// Publish through npm so the hosted member's packument advertises this
	// version. Group artifact ownership follows current packuments; an orphan
	// hosted tarball cannot shadow a later member's advertised release.
	publish := fmt.Sprintf(`{"name":"pkg","versions":{"1.0.0":{"name":"pkg","version":"1.0.0","dist":{}}},"_attachments":{"pkg-1.0.0.tgz":{"data":%q}}}`, base64.StdEncoding.EncodeToString(archive))
	response := fixture.request(t, http.MethodPut, "/repository/npm-hosted/pkg", []byte(publish), true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()
	hostedAssets := readAssets("/api/v1/repositories/npm-hosted/assets?prefix=pkg/-/")
	var hostedID int64
	for _, asset := range hostedAssets.Items {
		if asset.Path == "pkg/-/pkg-1.0.0.tgz" {
			hostedID = asset.ID
		}
	}
	if hostedID == 0 {
		t.Fatalf("hosted npm publication lacks a tarball: %+v", hostedAssets.Items)
	}
	hostedPage := readAssets("/api/v1/repositories/npm-hosted-group/assets?prefix=pkg/-/")
	var hostedTarballs []domain.Asset
	for _, asset := range hostedPage.Items {
		if asset.Path == "pkg/-/pkg-1.0.0.tgz" || asset.FormatPath == "pkg/-/pkg-1.0.0.tgz" {
			hostedTarballs = append(hostedTarballs, asset)
		}
	}
	if len(hostedTarballs) != 1 || hostedTarballs[0].ID != hostedID {
		t.Fatalf("hosted member did not shadow opaque proxy asset: %+v", hostedPage.Items)
	}
	for _, suffix := range []string{"", "/download"} {
		response := fixture.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/repositories/npm-group/assets/%d%s", shadowedID, suffix), nil, true)
		assertStatus(t, response, http.StatusNotFound)
		response.Body.Close()
	}
	for _, path := range []string{
		"/api/v1/repositories/npm-group/browse?limit=1",
		"/api/v1/search?repository=npm-group&pathPrefix=" + url.QueryEscape("pkg/-/") + "&limit=1",
	} {
		seenTarballs := 0
		cursor := ""
		for pageNumber := 0; pageNumber < 8; pageNumber++ {
			requestPath := path
			if cursor != "" {
				requestPath += "&cursor=" + url.QueryEscape(cursor)
			}
			response := fixture.request(t, http.MethodGet, requestPath, nil, true)
			assertStatus(t, response, http.StatusOK)
			var page httpx.CollectionPage[repositoryBrowseItem]
			if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			for _, item := range page.Items {
				if item.Asset.FormatPath == "pkg/-/pkg-1.0.0.tgz" {
					seenTarballs++
					if item.Asset.ID != visibleID {
						t.Fatalf("%s: unexpected tarball %+v", path, item)
					}
				}
			}
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
		if cursor != "" || seenTarballs != 1 {
			t.Fatalf("%s: tarball count = %d, cursor = %q", path, seenTarballs, cursor)
		}
	}
	componentPath := "/api/v1/repositories/npm-group/components?limit=1"
	componentCursor := ""
	componentTarballs := 0
	for pageNumber := 0; pageNumber < 8; pageNumber++ {
		requestPath := componentPath
		if componentCursor != "" {
			requestPath += "&cursor=" + url.QueryEscape(componentCursor)
		}
		response := fixture.request(t, http.MethodGet, requestPath, nil, true)
		assertStatus(t, response, http.StatusOK)
		var componentPage httpx.CollectionPage[repositoryComponentVersion]
		if err := json.NewDecoder(response.Body).Decode(&componentPage); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		for _, item := range componentPage.Items {
			if item.AssetID == visibleID {
				componentTarballs++
			}
			if item.AssetID == shadowedID {
				t.Fatalf("shadowed asset visible in components: %+v", item)
			}
		}
		componentCursor = componentPage.NextCursor
		if componentCursor == "" {
			break
		}
	}
	if componentCursor != "" || componentTarballs != 1 {
		t.Fatalf("components: tarball count = %d, cursor = %q", componentTarballs, componentCursor)
	}
}
