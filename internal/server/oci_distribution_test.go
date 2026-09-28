package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/oci"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

func TestOCICatalogAndTagPagination(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, image := range []string{"acme/alpha", "acme/beta", "acme/gamma"} {
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository:  "oci",
			Path:        ocimodel.ManifestPath(image, "latest"),
			Digest:      testDigest([]byte(image)),
			Size:        12,
			ContentType: ociManifestMediaTypeForTest,
			Kind:        "oci-manifest",
			Reference:   "latest",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository:  "oci",
			Path:        ocimodel.ManifestPath(image, "stable"),
			Digest:      testDigest([]byte(image + "stable")),
			Size:        12,
			ContentType: ociManifestMediaTypeForTest,
			Kind:        "oci-manifest",
			Reference:   "stable",
		}); err != nil {
			t.Fatal(err)
		}
	}

	catalog := fixture.request(t, http.MethodGet, "/v2/_catalog", nil, true)
	assertStatus(t, catalog, http.StatusOK)
	assertJSONFieldStrings(t, catalog, "repositories", "acme/alpha", "acme/beta", "acme/gamma")

	firstPage := fixture.request(t, http.MethodGet, "/v2/_catalog?n=2", nil, true)
	assertStatus(t, firstPage, http.StatusOK)
	link := firstPage.Header.Get("Link")
	if !strings.Contains(link, `rel="next"`) || !strings.Contains(link, "last=acme%2Fbeta") {
		t.Fatalf("catalog Link = %q", link)
	}
	assertJSONFieldStrings(t, firstPage, "repositories", "acme/alpha", "acme/beta")

	nextPage := fixture.request(t, http.MethodGet, "/v2/_catalog?n=2&last=acme/beta", nil, true)
	assertStatus(t, nextPage, http.StatusOK)
	if nextPage.Header.Get("Link") != "" {
		t.Fatalf("unexpected catalog continuation Link %q", nextPage.Header.Get("Link"))
	}
	assertJSONFieldStrings(t, nextPage, "repositories", "acme/gamma")

	tags := fixture.request(t, http.MethodGet, "/v2/acme/alpha/tags/list?n=1", nil, true)
	assertStatus(t, tags, http.StatusOK)
	if !strings.Contains(tags.Header.Get("Link"), "last=latest") {
		t.Fatalf("tag list Link = %q", tags.Header.Get("Link"))
	}
	assertJSONFieldStrings(t, tags, "tags", "latest")

	remaining := fixture.request(
		t,
		http.MethodGet,
		"/v2/acme/alpha/tags/list?n=1&last=latest",
		nil,
		true,
	)
	assertStatus(t, remaining, http.StatusOK)
	assertJSONFieldStrings(t, remaining, "tags", "stable")

	invalid := fixture.request(t, http.MethodGet, "/v2/_catalog?n=bogus", nil, true)
	assertStatus(t, invalid, http.StatusBadRequest)
	assertOCIErrorCode(t, invalid, "UNSUPPORTED")

	oversized := fixture.request(t, http.MethodGet, "/v2/_catalog?n=10000", nil, true)
	assertStatus(t, oversized, http.StatusBadRequest)
	assertOCIErrorCode(t, oversized, "UNSUPPORTED")
}

func TestOCIDigestDeleteRemovesSameImageTagsAndAllowsReclamation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	const mediaType = "application/vnd.oci.image.index.v1+json"

	push := func(path string) string {
		t.Helper()
		response := fixture.requestWithContentType(
			t,
			http.MethodPut,
			path,
			manifest,
			mediaType,
			true,
		)
		assertStatus(t, response, http.StatusCreated)
		digest := response.Header.Get("Docker-Content-Digest")
		response.Body.Close()
		return digest
	}
	get := func(path string, expected int) {
		t.Helper()
		response := fixture.request(t, http.MethodGet, path, nil, true)
		assertStatus(t, response, expected)
		response.Body.Close()
	}
	remove := func(path string) {
		t.Helper()
		response := fixture.request(t, http.MethodDelete, path, nil, true)
		assertStatus(t, response, http.StatusAccepted)
		response.Body.Close()
	}

	appLatest := "/repository/oci/v2/acme/app/manifests/latest"
	appStable := "/repository/oci/v2/acme/app/manifests/stable"
	otherLatest := "/repository/oci/v2/acme/other/manifests/latest"
	digest := push(appLatest)
	if got := push(appStable); got != digest {
		t.Fatalf("stable digest = %q, want %q", got, digest)
	}
	if got := push(otherLatest); got != digest {
		t.Fatalf("other image digest = %q, want %q", got, digest)
	}

	remove("/repository/oci/v2/acme/app/manifests/" + digest)
	get("/repository/oci/v2/acme/app/manifests/"+digest, http.StatusNotFound)
	get(appLatest, http.StatusNotFound)
	get(appStable, http.StatusNotFound)
	get(otherLatest, http.StatusOK)

	// The equal manifest in another image namespace still retains the blob.
	if _, err := fixture.Handler.runGarbageCollection(
		context.Background(),
		false,
		0,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Handler.blobs.Head(context.Background(), digest); err != nil {
		t.Fatalf("other image no longer retained the shared manifest: %v", err)
	}

	remove("/repository/oci/v2/acme/other/manifests/" + digest)
	get(otherLatest, http.StatusNotFound)
	if _, err := fixture.Handler.runGarbageCollection(
		context.Background(),
		false,
		0,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Handler.blobs.Head(
		context.Background(),
		digest,
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted manifest blob error = %v, want not found", err)
	}
}

func TestOCIGroupAndProxyCatalog(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository:  "oci",
		Path:        ocimodel.ManifestPath("hosted/app", "v1"),
		Digest:      testDigest([]byte("hosted")),
		Size:        8,
		ContentType: ociManifestMediaTypeForTest,
		Kind:        "oci-manifest",
		Reference:   "v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "oci-upstream",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:    "oci-group",
		Format:  "oci",
		Type:    "group",
		Members: []string{"oci", "oci-upstream"},
	}); err != nil {
		t.Fatal(err)
	}

	var catalogPath string
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			catalogPath = request.URL.Path
			if request.URL.RawQuery != "" {
				t.Fatalf("unexpected catalog query %q", request.URL.RawQuery)
			}
			return testHTTPResponse(request, http.StatusOK, `{"repositories":["upstream/app"]}`), nil
		}),
	})

	response := fixture.request(t, http.MethodGet, "/repository/oci-group/v2/_catalog", nil, true)
	assertStatus(t, response, http.StatusOK)
	if catalogPath != "/v2/_catalog" {
		t.Fatalf("upstream catalog path = %q", catalogPath)
	}
	assertJSONFieldStrings(t, response, "repositories", "hosted/app", "upstream/app")
}

func TestOCIProxyFollowsPrefixedUpstreamCatalogAndTagPages(t *testing.T) {
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "oci-proxy", Format: "oci", Type: "proxy",
		Upstream: "https://registry.example/repository/source",
	}); err != nil {
		t.Fatal(err)
	}
	var requests []string
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.RequestURI())
		var body, next string
		switch request.URL.RequestURI() {
		case "/repository/source/v2/_catalog":
			body = `{"repositories":["acme/first"]}`
			next = "https://registry.example/repository/source/v2/_catalog?n=1&last=acme%2Ffirst"
		case "/repository/source/v2/_catalog?n=1&last=acme%2Ffirst":
			body = `{"repositories":["acme/second"]}`
		case "/repository/source/v2/acme/app/tags/list":
			body = `{"tags":["first"]}`
			next = "https://registry.example/repository/source/v2/acme/app/tags/list?n=1&last=first"
		case "/repository/source/v2/acme/app/tags/list?n=1&last=first":
			body = `{"tags":["second"]}`
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
		response := testHTTPResponse(request, http.StatusOK, body)
		if next != "" {
			response.Header.Add("Link", "</previous>; rel=\"prev\"")
			response.Header.Add("Link", "<"+next+">; rel=\"next\"")
		}
		return response, nil
	})})

	catalog := fixture.request(t, http.MethodGet, "/repository/oci-proxy/v2/_catalog", nil, true)
	assertStatus(t, catalog, http.StatusOK)
	assertJSONFieldStrings(t, catalog, "repositories", "acme/first", "acme/second")
	tags := fixture.request(t, http.MethodGet, "/repository/oci-proxy/v2/acme/app/tags/list", nil, true)
	assertStatus(t, tags, http.StatusOK)
	assertJSONFieldStrings(t, tags, "tags", "first", "second")
	if len(requests) != 4 {
		t.Fatalf("upstream requests = %v, want two catalog and two tag pages", requests)
	}
}

func TestOCIProxyFollowsPrefixedUpstreamReferrerPages(t *testing.T) {
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "oci-proxy", Format: "oci", Type: "proxy",
		Upstream: "https://registry.example/repository/source",
	}); err != nil {
		t.Fatal(err)
	}
	subject := "sha256:" + strings.Repeat("a", 64)
	first := "sha256:" + strings.Repeat("b", 64)
	second := "sha256:" + strings.Repeat("c", 64)
	artifactType := "application/vnd.example.sbom"
	basePath := "/repository/source/v2/acme/app/referrers/" + subject
	firstQuery := "artifactType=application%2Fvnd.example.sbom"
	var requests []string
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.RequestURI())
		var digest, link string
		switch request.URL.RequestURI() {
		case basePath + "?" + firstQuery:
			digest = first
			link = "https://registry.example" + basePath + "?" + firstQuery + "&page=2"
		case basePath + "?" + firstQuery + "&page=2":
			digest = second
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
		body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"digest":%q,"artifactType":%q}]}`,
			oci.IndexMediaType, digest, artifactType)
		response := testHTTPResponse(request, http.StatusOK, body)
		if link != "" {
			response.Header.Add("Link", "</previous>; rel=\"prev\"")
			response.Header.Add("Link", "<"+link+">; rel=\"next\"")
		}
		return response, nil
	})})
	path := "/repository/oci-proxy/v2/acme/app/referrers/" + subject + "?" + firstQuery
	response := fixture.request(t, http.MethodGet, path, nil, true)
	assertStatus(t, response, http.StatusOK)
	var index oci.Index
	if err := json.NewDecoder(response.Body).Decode(&index); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(index.Manifests) != 2 || index.Manifests[0].Digest != first ||
		index.Manifests[1].Digest != second || len(requests) != 2 {
		t.Fatalf("referrers = %+v, upstream requests = %v", index.Manifests, requests)
	}
}

func TestOCIProxyRejectsCatalogPastUpstreamPageLimit(t *testing.T) {
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "oci-proxy", Format: "oci", Type: "proxy",
		Upstream: "https://registry.example/repository/source",
	}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		response := testHTTPResponse(request, http.StatusOK, `{"repositories":["acme/app"]}`)
		response.Header.Set("Link", "</repository/source/v2/_catalog?last=acme%2Fapp>; rel=\"next\"")
		return response, nil
	})})
	response := fixture.request(t, http.MethodGet, "/repository/oci-proxy/v2/_catalog", nil, true)
	assertStatus(t, response, http.StatusBadGateway)
	response.Body.Close()
	if requests != 32 {
		t.Fatalf("upstream calls = %d, want bounded 32", requests)
	}
}

func TestOCIReferrersArtifactTypeFilter(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	subjectDigest := "sha256:" + strings.Repeat("a", 64)
	signatureDigest := "sha256:" + strings.Repeat("b", 64)
	sbomDigest := "sha256:" + strings.Repeat("c", 64)
	signatureType := "application/vnd.dev.cosign.artifact.sig.v1+json"
	sbomType := "application/vnd.example.sbom"

	for _, asset := range []domain.Asset{
		{
			Repository:    "oci",
			Path:          ocimodel.ManifestPath("acme/app", signatureDigest),
			Digest:        signatureDigest,
			Size:          20,
			ContentType:   ociManifestMediaTypeForTest,
			Kind:          "oci-manifest",
			Reference:     signatureDigest,
			SubjectDigest: subjectDigest,
			Attributes:    assetattrs.SetOCIArtifactType(nil, signatureType),
		},
		{
			Repository:    "oci",
			Path:          ocimodel.ManifestPath("acme/app", sbomDigest),
			Digest:        sbomDigest,
			Size:          30,
			ContentType:   ociManifestMediaTypeForTest,
			Kind:          "oci-manifest",
			Reference:     sbomDigest,
			SubjectDigest: subjectDigest,
			Attributes:    assetattrs.SetOCIArtifactType(nil, sbomType),
		},
	} {
		if _, err := fixture.Metadata.PutAsset(context.Background(), asset); err != nil {
			t.Fatal(err)
		}
	}

	filtered := fixture.request(
		t,
		http.MethodGet,
		"/v2/acme/app/referrers/"+subjectDigest+"?artifactType="+url.QueryEscape(signatureType),
		nil,
		true,
	)
	assertStatus(t, filtered, http.StatusOK)
	if filtered.Header.Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatalf("OCI-Filters-Applied = %q", filtered.Header.Get("OCI-Filters-Applied"))
	}
	var index oci.Index
	if err := json.NewDecoder(filtered.Body).Decode(&index); err != nil {
		t.Fatal(err)
	}
	filtered.Body.Close()
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != signatureDigest {
		t.Fatalf("unexpected filtered referrers: %+v", index.Manifests)
	}
	if index.Manifests[0].ArtifactType != signatureType {
		t.Fatalf("missing artifactType on descriptor: %+v", index.Manifests[0])
	}

	unfiltered := fixture.request(
		t,
		http.MethodGet,
		"/v2/acme/app/referrers/"+subjectDigest,
		nil,
		true,
	)
	assertStatus(t, unfiltered, http.StatusOK)
	if unfiltered.Header.Get("OCI-Filters-Applied") != "" {
		t.Fatal("unfiltered referrers set OCI-Filters-Applied")
	}
	if err := json.NewDecoder(unfiltered.Body).Decode(&index); err != nil {
		t.Fatal(err)
	}
	unfiltered.Body.Close()
	if len(index.Manifests) != 2 {
		t.Fatalf("unfiltered referrers = %+v", index.Manifests)
	}
}

func TestOCIProxyReferrersForwardsArtifactType(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "oci-upstream",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	}); err != nil {
		t.Fatal(err)
	}
	subjectDigest := "sha256:" + strings.Repeat("d", 64)
	artifactType := "application/vnd.dev.cosign.artifact.sig.v1+json"
	var upstreamQuery string
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamQuery = request.URL.RawQuery
			payload := fmt.Sprintf(
				`{"schemaVersion":2,"mediaType":%q,"manifests":[]}`,
				oci.IndexMediaType,
			)
			return testHTTPResponse(request, http.StatusOK, payload), nil
		}),
	})

	response := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci-upstream/v2/acme/app/referrers/"+subjectDigest+"?artifactType="+url.QueryEscape(artifactType),
		nil,
		true,
	)
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	wantQuery := url.Values{"artifactType": {artifactType}}.Encode()
	if upstreamQuery != wantQuery {
		t.Fatalf("upstream query = %q, want %q", upstreamQuery, wantQuery)
	}
}

func TestOCIUploadContentRangeAndBlobRange(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	start := fixture.request(t, http.MethodPost, "/v2/acme/app/blobs/uploads/", nil, true)
	assertStatus(t, start, http.StatusAccepted)
	location := start.Header.Get("Location")
	start.Body.Close()

	mismatch := requestWithHeaders(
		t,
		fixture,
		http.MethodPatch,
		location,
		[]byte("hello"),
		map[string]string{"Content-Range": "4-8"},
	)
	assertStatus(t, mismatch, http.StatusRequestedRangeNotSatisfiable)
	assertOCIErrorCode(t, mismatch, "BLOB_UPLOAD_INVALID")
	if got := mismatch.Header.Get("Range"); got != "0-0" {
		t.Fatalf("mismatch Range = %q, want 0-0", got)
	}

	first := requestWithHeaders(
		t,
		fixture,
		http.MethodPatch,
		location,
		[]byte("hello"),
		map[string]string{"Content-Range": "0-4"},
	)
	assertStatus(t, first, http.StatusAccepted)
	if got := first.Header.Get("Range"); got != "0-4" {
		t.Fatalf("first chunk Range = %q, want 0-4", got)
	}
	first.Body.Close()

	content := []byte("hello world")
	wrongFinal := requestWithHeaders(
		t,
		fixture,
		http.MethodPut,
		location+"?digest="+testDigest(content),
		[]byte(" world"),
		map[string]string{"Content-Range": "6-11"},
	)
	assertStatus(t, wrongFinal, http.StatusRequestedRangeNotSatisfiable)
	assertOCIErrorCode(t, wrongFinal, "BLOB_UPLOAD_INVALID")
	if got := wrongFinal.Header.Get("Range"); got != "0-4" {
		t.Fatalf("finalization mismatch Range = %q, want 0-4", got)
	}

	completed := requestWithHeaders(
		t,
		fixture,
		http.MethodPut,
		location+"?digest="+testDigest(content),
		[]byte(" world"),
		map[string]string{"Content-Range": "5-10"},
	)
	assertStatus(t, completed, http.StatusCreated)
	completed.Body.Close()

	blobPath := "/v2/acme/app/blobs/" + testDigest(content)
	head := fixture.request(t, http.MethodHead, blobPath, nil, true)
	assertStatus(t, head, http.StatusOK)
	if head.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", head.Header.Get("Accept-Ranges"))
	}
	head.Body.Close()

	partial := requestWithHeaders(
		t,
		fixture,
		http.MethodGet,
		blobPath,
		nil,
		map[string]string{"Range": "bytes=0-4"},
	)
	assertStatus(t, partial, http.StatusPartialContent)
	if partial.Header.Get("Content-Range") != "bytes 0-4/11" {
		t.Fatalf("Content-Range = %q", partial.Header.Get("Content-Range"))
	}
	assertBody(t, partial, []byte("hello"))

	unsatisfiable := requestWithHeaders(
		t,
		fixture,
		http.MethodGet,
		blobPath,
		nil,
		map[string]string{"Range": "bytes=20-30"},
	)
	assertStatus(t, unsatisfiable, http.StatusRequestedRangeNotSatisfiable)
	unsatisfiable.Body.Close()
}

func TestOCICrossRepositoryBlobMount(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	content := []byte("shared-layer")
	digest := testDigest(content)
	upload := fixture.request(
		t,
		http.MethodPost,
		"/v2/acme/source/blobs/uploads/?digest="+digest,
		content,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	mounted := fixture.request(
		t,
		http.MethodPost,
		"/v2/acme/dest/blobs/uploads/?mount="+digest+"&from=acme/source",
		nil,
		true,
	)
	assertStatus(t, mounted, http.StatusCreated)
	if mounted.Header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("mounted digest = %q", mounted.Header.Get("Docker-Content-Digest"))
	}
	mounted.Body.Close()

	pulled := fixture.request(t, http.MethodGet, "/v2/acme/dest/blobs/"+digest, nil, true)
	assertStatus(t, pulled, http.StatusOK)
	assertBody(t, pulled, content)

	missing := fixture.request(
		t,
		http.MethodPost,
		"/v2/acme/dest/blobs/uploads/?mount="+testDigest([]byte("absent"))+"&from=acme/source",
		nil,
		true,
	)
	assertStatus(t, missing, http.StatusAccepted)
	missing.Body.Close()
}

func pageFromQuery(t *testing.T, n string) oci.PageRequest {
	t.Helper()
	values := url.Values{}
	if n != "" {
		values.Set("n", n)
	}
	page, err := oci.ParsePageRequest(values)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func requestWithHeaders(
	t *testing.T,
	fixture *serverFixture,
	method string,
	requestPath string,
	body []byte,
	headers map[string]string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func assertJSONFieldStrings(t *testing.T, response *http.Response, field string, want ...string) {
	t.Helper()
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	raw, _ := payload[field].([]any)
	got := make([]string, 0, len(raw))
	for _, value := range raw {
		got = append(got, fmt.Sprint(value))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
}
