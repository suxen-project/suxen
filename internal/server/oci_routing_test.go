package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

func TestOCIProxyClassifiesManifestWithBlobsInImageName(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	manifest := []byte(`{
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "layers": []
    }`)
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v2/acme/blobs/cache/manifests/latest" {
				t.Fatalf("unexpected upstream path %q", request.URL.Path)
			}
			response := testHTTPResponse(request, http.StatusOK, string(manifest))
			response.Header.Set("Content-Type", ociManifestMediaTypeForTest)
			return response, nil
		}),
	})
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "ambiguous-proxy",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(
		t,
		http.MethodGet,
		"/repository/ambiguous-proxy/v2/acme/blobs/cache/manifests/latest",
		nil,
		true,
	)
	assertStatus(t, response, http.StatusOK)
	assertBody(t, response, manifest)

	asset, err := fixture.Metadata.Asset(
		context.Background(),
		"ambiguous-proxy",
		"v2/acme/blobs/cache/manifests/latest",
	)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Kind != "oci-manifest" || asset.Reference != "latest" {
		t.Fatalf("unexpected cached manifest metadata: %+v", asset)
	}
}

func TestOCIHostedRejectsOversizedManifest(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	body := bytes.Repeat([]byte(" "), int(ocimodel.MaxManifestBytes)+1)

	tests := []struct {
		name          string
		reference     string
		contentLength int64
	}{
		{name: "known content length", reference: "known", contentLength: int64(len(body))},
		{name: "streamed body", reference: "streamed", contentLength: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPut,
				"/repository/oci/v2/acme/application/manifests/"+test.reference,
				bytes.NewReader(body),
			)
			request.ContentLength = test.contentLength
			request.Header.Set("Authorization", "Bearer "+testToken)
			request.Header.Set("Content-Type", ociManifestMediaTypeForTest)
			recorder := httptest.NewRecorder()

			fixture.Handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			assertStatus(t, response, http.StatusRequestEntityTooLarge)
			assertOCIErrorCode(t, response, "MANIFEST_INVALID")

			_, err := fixture.Metadata.Asset(
				context.Background(),
				"oci",
				ocimodel.ManifestPath("acme/application", test.reference),
			)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("oversized manifest was stored: %v", err)
			}
		})
	}
}

func TestOCIHostedRejectsInvalidManifestEnvelope(t *testing.T) {
	fixture := newServerFixture(t)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "JSON null", body: `null`},
		{name: "schema version one", body: `{"schemaVersion":1,"manifests":[]}`},
		{name: "trailing JSON", body: `{"schemaVersion":2,"manifests":[]}{}`},
		{name: "empty subject digest", body: `{"schemaVersion":2,"subject":{}}`},
		{name: "malformed subject digest", body: `{"schemaVersion":2,"subject":{"digest":"sha256:short"}}`},
		{name: "missing config digest", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":0},"layers":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reference := strings.ReplaceAll(test.name, " ", "-")
			response := fixture.requestWithContentType(
				t, http.MethodPut,
				"/repository/oci/v2/acme/app/manifests/"+reference,
				[]byte(test.body), ociManifestMediaTypeForTest, true,
			)
			assertStatus(t, response, http.StatusBadRequest)
			assertOCIErrorCode(t, response, "MANIFEST_INVALID")
			response.Body.Close()
			_, err := fixture.Metadata.Asset(
				context.Background(), "oci", ocimodel.ManifestPath("acme/app", reference),
			)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("invalid manifest was stored: %v", err)
			}
		})
	}
}

func TestOCIHostedRejectsInvalidImageNamesAndManifestReferences(t *testing.T) {
	fixture := newServerFixture(t)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	for _, test := range []struct {
		name string
		path string
		code string
	}{
		{name: "space in tag", path: "/repository/oci/v2/acme/app/manifests/bad%20tag", code: "TAG_INVALID"},
		{name: "tag too long", path: "/repository/oci/v2/acme/app/manifests/" + strings.Repeat("a", 129), code: "TAG_INVALID"},
		{name: "uppercase image name", path: "/repository/oci/v2/Acme/app/manifests/latest", code: "NAME_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.requestWithContentType(t, http.MethodPut, test.path, manifest,
				"application/vnd.oci.image.index.v1+json", true)
			assertStatus(t, response, http.StatusBadRequest)
			assertOCIErrorCode(t, response, test.code)
			response.Body.Close()
		})
	}
}

func TestOCIHostedRejectsDescriptorSizeMismatch(t *testing.T) {
	fixture := newServerFixture(t)
	config := []byte(`{}`)
	digest := testDigest(config)
	response := fixture.request(t, http.MethodPost,
		"/repository/oci/v2/acme/app/blobs/uploads/?digest="+digest, config, true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digest + `","size":3},"layers":[]}`)
	response = fixture.requestWithContentType(t, http.MethodPut,
		"/repository/oci/v2/acme/app/manifests/wrong-size", manifest,
		"application/vnd.oci.image.manifest.v1+json", true)
	assertStatus(t, response, http.StatusBadRequest)
	assertOCIErrorCode(t, response, "MANIFEST_INVALID")
	response.Body.Close()
}

func TestOCIProxyRejectsOversizedManifestWithoutCachingIt(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	body := strings.Repeat(" ", int(ocimodel.MaxManifestBytes)+1)
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			response := &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}
			response.Header.Set("Content-Type", ociManifestMediaTypeForTest)
			return response, nil
		}),
	})
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "large-manifest-proxy",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	}); err != nil {
		t.Fatal(err)
	}

	requestPath := "/repository/large-manifest-proxy/v2/acme/application/manifests/latest"
	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(t, http.MethodGet, requestPath, nil, true)
		assertStatus(t, response, http.StatusBadGateway)
		var envelope struct {
			Errors []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
		closeErr := response.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNKNOWN" {
			t.Fatalf("got OCI errors %+v, want UNKNOWN", envelope.Errors)
		}
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream called %d times, want 2 uncached failures", upstreamCalls)
	}

	_, err := fixture.Metadata.Asset(
		context.Background(),
		"large-manifest-proxy",
		"v2/acme/application/manifests/latest",
	)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("oversized upstream manifest was cached: %v", err)
	}
}

func assertOCIErrorCode(t *testing.T, response *http.Response, expected string) {
	t.Helper()
	defer response.Body.Close()
	var payload struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Errors) != 1 || payload.Errors[0].Code != expected {
		t.Fatalf("unexpected OCI error response: %+v", payload.Errors)
	}
}
