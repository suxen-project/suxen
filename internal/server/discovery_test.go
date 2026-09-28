package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestWhoAmIReportsAnonymousAndAuthenticatedIdentity(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	anonymous := fixture.request(t, http.MethodGet, "/api/v1/whoami", nil, false)
	assertStatus(t, anonymous, http.StatusOK)
	var anonymousIdentity identityResponse
	if err := json.NewDecoder(anonymous.Body).Decode(&anonymousIdentity); err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymousIdentity.Authenticated || anonymousIdentity.AuthenticationKind != "anonymous" {
		t.Fatalf("unexpected anonymous identity: %+v", anonymousIdentity)
	}
	if len(anonymousIdentity.Roles) != 1 || anonymousIdentity.Roles[0] != "anonymous" {
		t.Fatalf("unexpected anonymous roles: %+v", anonymousIdentity.Roles)
	}

	authenticated := fixture.request(t, http.MethodGet, "/api/v1/whoami", nil, true)
	assertStatus(t, authenticated, http.StatusOK)
	body, err := io.ReadAll(authenticated.Body)
	if err != nil {
		t.Fatal(err)
	}
	authenticated.Body.Close()
	if strings.Contains(string(body), testToken) {
		t.Fatal("whoami response exposed an API token")
	}
	var identity identityResponse
	if err := json.Unmarshal(body, &identity); err != nil {
		t.Fatal(err)
	}
	if !identity.Authenticated || identity.Username != "admin" || !identity.Administrator {
		t.Fatalf("unexpected authenticated identity: %+v", identity)
	}
	if identity.AuthenticationKind != "api-token" || len(identity.EffectivePrivileges) != 1 ||
		identity.EffectivePrivileges[0] != "*" {
		t.Fatalf("unexpected authenticated authorization: %+v", identity)
	}

	invalid := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/whoami",
		nil,
		"",
		"not-a-valid-token",
	)
	assertStatus(t, invalid, http.StatusUnauthorized)
	invalid.Body.Close()
}

func TestEffectiveScopedPrivilegesIntersectsWildcardSegments(t *testing.T) {
	tests := []struct {
		name       string
		privileges []string
		scopes     []string
		want       []string
	}{
		{
			name:       "crossing wildcards",
			privileges: []string{"repository:*:read"},
			scopes:     []string{"repository:raw:*"},
			want:       []string{"repository:raw:read"},
		},
		{
			name:       "administrator constrained",
			privileges: []string{"*"},
			scopes:     []string{"repository:raw:read"},
			want:       []string{"repository:raw:read"},
		},
		{
			name:       "incompatible actions",
			privileges: []string{"repository:*:read"},
			scopes:     []string{"repository:raw:write"},
			want:       []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sortedUnique(effectiveScopedPrivileges(test.privileges, test.scopes))
			if !slices.Equal(got, test.want) {
				t.Fatalf("effective privileges = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCollectionPaginationRejectsMalformedAndStaleCursors(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	first := fixture.request(t, http.MethodGet, "/api/v1/repositories?limit=1", nil, true)
	assertStatus(t, first, http.StatusOK)
	var page httpx.CollectionPage[domain.Repository]
	if err := json.NewDecoder(first.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", page)
	}

	secondPath := "/api/v1/repositories?limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	second := fixture.request(t, http.MethodGet, secondPath, nil, true)
	assertStatus(t, second, http.StatusOK)
	var secondPage httpx.CollectionPage[domain.Repository]
	if err := json.NewDecoder(second.Body).Decode(&secondPage); err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if len(secondPage.Items) != 1 || secondPage.Items[0].Name == page.Items[0].Name {
		t.Fatalf("pagination did not advance deterministically: %+v", secondPage)
	}

	malformed := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories?cursor=not-base64!",
		nil,
		true,
	)
	assertStatus(t, malformed, http.StatusBadRequest)
	malformed.Body.Close()

	trailingPayload, err := json.Marshal(httpx.CursorPayload{
		Version: 1, Resource: "repositories", Offset: 1, Snapshot: "snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	trailingCursor := base64.RawURLEncoding.EncodeToString(append(trailingPayload, []byte(`{}`)...))
	trailing := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories?cursor="+url.QueryEscape(trailingCursor),
		nil,
		true,
	)
	assertStatus(t, trailing, http.StatusBadRequest)
	trailing.Body.Close()

	missingSnapshot := httpx.EncodeCursor(httpx.CursorPayload{
		Version: 1, Resource: "repositories", Offset: 1,
	})
	missing := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories?cursor="+url.QueryEscape(missingSnapshot),
		nil,
		true,
	)
	assertStatus(t, missing, http.StatusBadRequest)
	missing.Body.Close()

	for name, invalidCursor := range map[string]string{
		"padding":  page.NextCursor + "=",
		"overlong": strings.Repeat("A", httpx.MaximumCursorLength+1),
		"noncanonical": base64.RawURLEncoding.EncodeToString([]byte(
			`{"v":1, "r":"repositories","o":1,"s":"snapshot"}`,
		)),
	} {
		t.Run(name, func(t *testing.T) {
			response := fixture.request(
				t,
				http.MethodGet,
				"/api/v1/repositories?cursor="+url.QueryEscape(invalidCursor),
				nil,
				true,
			)
			assertStatus(t, response, http.StatusBadRequest)
			response.Body.Close()
		})
	}

	wrongCollection := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/users?cursor="+url.QueryEscape(page.NextCursor),
		nil,
		true,
	)
	assertStatus(t, wrongCollection, http.StatusConflict)
	wrongCollection.Body.Close()

	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "added", Format: "raw", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	stale := fixture.request(t, http.MethodGet, secondPath, nil, true)
	assertStatus(t, stale, http.StatusConflict)
	stale.Body.Close()

	tooLarge := fixture.request(t, http.MethodGet, "/api/v1/repositories?limit=201", nil, true)
	assertStatus(t, tooLarge, http.StatusBadRequest)
	tooLarge.Body.Close()
}

func TestBrowseAndSearchRespectRepositoryReadPrivileges(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()

	if err := fixture.Metadata.UpdateRole(ctx, domain.Role{
		Name: "anonymous", Description: "No anonymous access", Privileges: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name: "raw-reader", Privileges: []string{"repository:raw:read"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateUser(ctx, "reader", "reader-password", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "reader", []string{"raw-reader"}); err != nil {
		t.Fatal(err)
	}
	const readerToken = "reader-token-secret"
	if _, err := fixture.Metadata.CreateToken(ctx, "reader", "read", readerToken, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name: "private", Format: "raw", Type: "hosted", BlobStore: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "raw", Path: "releases/tool-1.2.3.zip", Digest: "sha256:raw", Size: 12,
		Attributes: map[string]any{
			"vuln.trivy":    map[string]any{"severity": "critical"},
			"sys.blobStore": "forged",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "private", Path: "secret/internal.bin", Digest: "sha256:private", Size: 99,
	}); err != nil {
		t.Fatal(err)
	}

	browse := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/repositories/raw/browse",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, browse, http.StatusOK)
	var browsePage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(browse.Body).Decode(&browsePage); err != nil {
		t.Fatal(err)
	}
	browse.Body.Close()
	if len(browsePage.Items) != 1 || browsePage.Items[0].Component != "releases" ||
		browsePage.Items[0].Version != "tool-1.2.3.zip" {
		t.Fatalf("unexpected Raw browse hierarchy: %+v", browsePage.Items)
	}

	searchPath := "/api/v1/search?q=tool&attribute=sys.blobStore%3Ddefault"
	search := fixture.requestWithBearer(t, http.MethodGet, searchPath, nil, "", readerToken)
	assertStatus(t, search, http.StatusOK)
	var searchPage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(search.Body).Decode(&searchPage); err != nil {
		t.Fatal(err)
	}
	search.Body.Close()
	if len(searchPage.Items) != 1 || searchPage.Items[0].Repository != "raw" {
		t.Fatalf("unexpected search results: %+v", searchPage.Items)
	}
	dottedSearch := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/search?attribute=vuln.trivy.severity%3Dcritical",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, dottedSearch, http.StatusOK)
	var dottedPage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(dottedSearch.Body).Decode(&dottedPage); err != nil {
		t.Fatal(err)
	}
	dottedSearch.Body.Close()
	if len(dottedPage.Items) != 1 || dottedPage.Items[0].Asset.Path != "releases/tool-1.2.3.zip" {
		t.Fatalf("dotted namespace search returned %+v", dottedPage.Items)
	}
	forgedSearch := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/search?attribute=sys.blobStore%3Dforged",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, forgedSearch, http.StatusOK)
	var forgedPage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(forgedSearch.Body).Decode(&forgedPage); err != nil {
		t.Fatal(err)
	}
	forgedSearch.Body.Close()
	if len(forgedPage.Items) != 0 {
		t.Fatalf("flat reserved collision matched search: %+v", forgedPage.Items)
	}

	noLeak := fixture.requestWithBearer(t, http.MethodGet, "/api/v1/search?q=secret", nil, "", readerToken)
	assertStatus(t, noLeak, http.StatusOK)
	noLeakBody, err := io.ReadAll(noLeak.Body)
	if err != nil {
		t.Fatal(err)
	}
	noLeak.Body.Close()
	if strings.Contains(string(noLeakBody), "private") || strings.Contains(string(noLeakBody), "internal.bin") {
		t.Fatalf("search leaked an unreadable repository: %s", noLeakBody)
	}

	denied := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/repositories/private/browse",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, denied, http.StatusForbidden)
	denied.Body.Close()

	readable := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/repositories/raw",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, readable, http.StatusOK)
	readable.Body.Close()

	hidden := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/repositories/private",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, hidden, http.StatusForbidden)
	hidden.Body.Close()

	listed := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/repositories",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, listed, http.StatusOK)
	var listedPage httpx.CollectionPage[domain.Repository]
	if err := json.NewDecoder(listed.Body).Decode(&listedPage); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(listedPage.Items) != 1 || listedPage.Items[0].Name != "raw" {
		t.Fatalf("repository list leaked unreadable repositories: %+v", listedPage.Items)
	}

	malformedSearch := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/search?attribute=sys..blobStore%3Ddefault",
		nil,
		"",
		readerToken,
	)
	assertStatus(t, malformedSearch, http.StatusBadRequest)
	malformedSearch.Body.Close()
}

func TestBrowseRepositoryDescriptorsDoNotLeakConfiguration(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateBlobStore(ctx, domain.BlobStore{
		Name:   "group-store",
		Driver: "fs",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_GROUP_STORE",
		},
		PhysicalIdentity: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.UpdateRole(ctx, domain.Role{
		Name: "anonymous", Description: "No anonymous access", Privileges: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []domain.Repository{
		{Name: "hidden-member", Format: "raw", Type: "hosted"},
		{
			Name: "cache", Format: "raw", Type: "proxy",
			Upstream: "https://user:password@upstream-secret.example/artifacts",
		},
		{
			Name:      "catalog",
			Format:    "raw",
			Type:      "group",
			BlobStore: "group-store",
			Members:   []string{"hidden-member"},
		},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "hidden-member",
		Path:       "logical/component-1.0.zip",
		Digest:     "sha256:abc123",
		Size:       42,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name: "catalog-reader",
		Privileges: []string{
			"repository:cache:read",
			"repository:catalog:read",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateUser(ctx, "catalog-user", "password", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "catalog-user", []string{"catalog-reader"}); err != nil {
		t.Fatal(err)
	}
	const token = "catalog-user-token"
	if _, err := fixture.Metadata.CreateToken(ctx, "catalog-user", "browse", token, nil); err != nil {
		t.Fatal(err)
	}

	response := fixture.requestWithBearer(t, http.MethodGet, "/api/v1/browse", nil, "", token)
	assertStatus(t, response, http.StatusOK)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for _, secret := range []string{"hidden-member", "upstream-secret.example", "password", "members", "upstream"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("browse descriptor leaked %q: %s", secret, body)
		}
	}
	var page httpx.CollectionPage[browseRepositoryDescriptor]
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "cache" || page.Items[1].Name != "catalog" {
		t.Fatalf("unexpected safe browse descriptors: %+v", page.Items)
	}

	groupBrowse := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/repositories/catalog/browse", nil, "", token,
	)
	assertStatus(t, groupBrowse, http.StatusOK)
	groupBody, err := io.ReadAll(groupBrowse.Body)
	if err != nil {
		t.Fatal(err)
	}
	groupBrowse.Body.Close()
	if strings.Contains(string(groupBody), "hidden-member") {
		t.Fatalf("group browse leaked its member identity: %s", groupBody)
	}
	var groupPage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.Unmarshal(groupBody, &groupPage); err != nil {
		t.Fatal(err)
	}
	if len(groupPage.Items) != 1 || groupPage.Items[0].Repository != "catalog" ||
		groupPage.Items[0].Asset.Repository != "catalog" {
		t.Fatalf("unexpected logical group browse: %+v", groupPage.Items)
	}
	system, _ := groupPage.Items[0].Asset.Attributes["sys"].(map[string]any)
	if system["repository"] != "catalog" ||
		system["type"] != "group" ||
		system["blobStore"] != "group-store" {
		t.Fatalf("group projection leaked physical metadata: %+v", system)
	}

	groupSearch := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/search?q=component-1.0", nil, "", token,
	)
	assertStatus(t, groupSearch, http.StatusOK)
	var searchPage httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(groupSearch.Body).Decode(&searchPage); err != nil {
		t.Fatal(err)
	}
	groupSearch.Body.Close()
	if len(searchPage.Items) != 1 || searchPage.Items[0].Repository != "catalog" {
		t.Fatalf("group-only search returned %+v", searchPage.Items)
	}
}

func TestLogicalGroupBrowseDeduplicatesPaths(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "first-member", Format: "raw", Type: "hosted"},
		{Name: "second-member", Format: "raw", Type: "hosted"},
		{
			Name:    "logical-group",
			Format:  "raw",
			Type:    "group",
			Members: []string{"first-member", "second-member"},
		},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}

	for _, asset := range []domain.Asset{
		{
			Repository: "first-member",
			Path:       "shared/component.zip",
			Digest:     "sha256:first",
		},
		{
			Repository: "second-member",
			Path:       "shared/component.zip",
			Digest:     "sha256:second",
		},
	} {
		if _, err := fixture.Metadata.PutAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}
	response := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories/logical-group/browse",
		nil,
		true,
	)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()
	var page httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("logical group browse returned %+v, want one deduplicated path", page.Items)
	}
	asset := page.Items[0].Asset
	if asset.Path != "shared/component.zip" || asset.Digest != "sha256:first" {
		t.Fatalf("logical group did not preserve first-member precedence: %+v", asset)
	}
}

func TestOCIBrowseUsesProjectedImageAndTagCoordinates(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository:  "oci",
		Path:        "v2/acme/widget/manifests/stable",
		Digest:      "sha256:manifest",
		ContentType: ociManifestMediaTypeForTest,
		Kind:        "oci-manifest",
		Reference:   "stable",
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories/oci/browse?component=acme%2Fwidget",
		nil,
		true,
	)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 1 {
		t.Fatalf("got OCI browse items %+v", page.Items)
	}
	item := page.Items[0]
	if item.Component != "acme/widget" || item.Version != "stable" ||
		item.Coordinates["image"] != "acme/widget" || item.Coordinates["tag"] != "stable" {
		t.Fatalf("unexpected OCI browse coordinates: %+v", item)
	}
}

func TestAssetPrefixTreatsLikeWildcardsLiterally(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, assetPath := range []string{"normal.bin", "%literal.bin", "_literal.bin"} {
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: "raw", Path: assetPath, Digest: "sha256:" + assetPath,
		}); err != nil {
			t.Fatal(err)
		}
	}

	response := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories/raw/assets?prefix=%25",
		nil,
		true,
	)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[domain.Asset]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Path != "%literal.bin" {
		t.Fatalf("LIKE wildcard was not treated literally: %+v", page.Items)
	}
}

func TestWhoAmIListsBrowserLoginProvidersOnlyWhenLoginIsEnabled(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, name := range []string{"corporate", "partner"} {
		if err := fixture.Metadata.CreateOIDCProvider(ctx, domain.OIDCProvider{
			Name:         name,
			Issuer:       "https://" + name + ".example",
			ClientID:     "suxen-ui",
			ClientSecret: "client-secret",
		}); err != nil {
			t.Fatal(err)
		}
	}

	whoami := func() identityResponse {
		response := fixture.request(t, http.MethodGet, "/api/v1/whoami", nil, false)
		assertStatus(t, response, http.StatusOK)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "client-secret") || strings.Contains(string(body), "suxen-ui") {
			t.Fatalf("whoami exposed provider credentials: %s", body)
		}
		var identity identityResponse
		if err := json.Unmarshal(body, &identity); err != nil {
			t.Fatal(err)
		}
		return identity
	}

	enabled := whoami()
	if len(enabled.LoginProviders) != 2 || enabled.LoginProviders[0] != "corporate" ||
		enabled.LoginProviders[1] != "partner" {
		t.Fatalf("login providers = %v, want corporate and partner", enabled.LoginProviders)
	}

	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.OIDCStateSecret = "" })
	disabled := whoami()
	if disabled.LoginProviders == nil || len(disabled.LoginProviders) != 0 {
		t.Fatalf("login providers without a state secret = %#v, want empty list", disabled.LoginProviders)
	}
}
