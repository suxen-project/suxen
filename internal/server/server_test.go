package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/oci"
	"github.com/suxen-project/suxen/internal/store"
)

const testToken = "test-administrator-token"

type serverFixture struct {
	Metadata *store.SQLStore
	Handler  *Server
}

func TestRawHostedDeduplicationAndGroupResolution(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	content := []byte("source archive contents")

	unauthorized := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/source.tar.gz",
		content,
		false,
	)
	assertStatus(t, unauthorized, http.StatusUnauthorized)

	first := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/source.tar.gz",
		content,
		true,
	)
	assertStatus(t, first, http.StatusCreated)
	first.Body.Close()

	second := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/source-copy.tar.gz",
		content,
		true,
	)
	assertStatus(t, second, http.StatusCreated)
	second.Body.Close()

	stats, err := fixture.Metadata.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Assets != 2 || stats.UniqueBlobs != 1 {
		t.Fatalf("unexpected deduplication stats: %+v", stats)
	}
	assetsResponse := fixture.request(
		t,
		http.MethodGet,
		"/api/v1/repositories/raw/assets?prefix=releases/",
		nil,
		true,
	)
	assertStatus(t, assetsResponse, http.StatusOK)
	var assetsPage httpx.CollectionPage[domain.Asset]
	if err := json.NewDecoder(assetsResponse.Body).Decode(&assetsPage); err != nil {
		t.Fatal(err)
	}
	assetsResponse.Body.Close()
	assets := assetsPage.Items
	if len(assets) != 2 {
		t.Fatalf("got %d listed assets, want 2", len(assets))
	}

	err = fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:    "public",
		Format:  "raw",
		Type:    "group",
		Members: []string{"raw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	groupRead := fixture.request(
		t,
		http.MethodGet,
		"/repository/public/releases/source.tar.gz",
		nil,
		true,
	)
	assertStatus(t, groupRead, http.StatusOK)
	assertBody(t, groupRead, content)
}

func TestRawProxyCachesAnUpstreamResponse(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	fixture := newServerFixture(t)
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("cached binary")),
				Request:    request,
			}, nil
		}),
	})
	err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "mirror",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://upstream.example/artifacts",
	})
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(
			t,
			http.MethodGet,
			"/repository/mirror/tool/linux-amd64",
			nil,
			true,
		)
		assertStatus(t, response, http.StatusOK)
		assertBody(t, response, []byte("cached binary"))
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream called %d times, want 1", upstreamCalls)
	}
}

func TestRawProxyNegativelyCachesNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upstreamCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
				Request:    request,
			}, nil
		}),
	})
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "negative",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://upstream.example/artifacts",
	}); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(
			t,
			http.MethodGet,
			"/repository/negative/missing",
			nil,
			true,
		)
		assertStatus(t, response, http.StatusNotFound)
		response.Body.Close()
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream called %d times, want one negatively cached request", upstreamCalls)
	}
}

func TestOCIProxyUsesBearerChallengeAndCachesManifest(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	manifest := []byte(`{
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "layers": []
    }`)
	registryCalls := 0
	tokenCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Host {
			case "auth.example":
				tokenCalls++
				if request.URL.Query().Get("scope") != "repository:acme/app:pull" {
					t.Fatalf("unexpected token scope %q", request.URL.Query().Get("scope"))
				}
				return testHTTPResponse(request, http.StatusOK, `{"token":"registry-token"}`), nil
			case "registry.example":
				registryCalls++
				if request.Header.Get("Authorization") == "Bearer registry-token" {
					response := testHTTPResponse(request, http.StatusOK, string(manifest))
					response.Header.Set("Content-Type", ociManifestMediaTypeForTest)
					return response, nil
				}
				response := testHTTPResponse(request, http.StatusUnauthorized, "")
				response.Header.Set(
					"WWW-Authenticate",
					`Bearer realm="https://auth.example/token",`+
						`service="registry.example",scope="repository:acme/app:pull"`,
				)
				return response, nil
			default:
				t.Fatalf("unexpected upstream host %q", request.URL.Host)
				return nil, nil
			}
		}),
	})
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "oci-mirror",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://registry.example",
	}); err != nil {
		t.Fatal(err)
	}

	requestPath := "/repository/oci-mirror/v2/acme/app/manifests/latest"
	for attempt := 0; attempt < 2; attempt++ {
		response := fixture.request(t, http.MethodGet, requestPath, nil, true)
		assertStatus(t, response, http.StatusOK)
		assertBody(t, response, manifest)
	}
	if registryCalls != 2 || tokenCalls != 1 {
		t.Fatalf("got registry calls=%d token calls=%d, want 2 and 1", registryCalls, tokenCalls)
	}

	cached, err := fixture.Metadata.Asset(
		context.Background(),
		"oci-mirror",
		"v2/acme/app/manifests/latest",
	)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Kind != "oci-manifest" || cached.Reference != "latest" {
		t.Fatalf("unexpected cached manifest metadata: %+v", cached)
	}
}

func TestOCIGroupMergesHostedAndProxyTagsAndReferrers(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	subjectDigest := "sha256:" + strings.Repeat("a", 64)
	localReferrerDigest := "sha256:" + strings.Repeat("b", 64)
	proxyReferrerDigest := "sha256:" + strings.Repeat("c", 64)

	for _, repository := range []domain.Repository{
		{Name: "oci-private", Format: "oci", Type: "hosted"},
		{
			Name:     "oci-upstream",
			Format:   "oci",
			Type:     "proxy",
			Upstream: "https://registry.example",
		},
		{
			Name:    "oci-public",
			Format:  "oci",
			Type:    "group",
			Members: []string{"oci-private", "oci-upstream"},
		},
	} {
		if err := fixture.Metadata.CreateRepository(context.Background(), repository); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository:  "oci-private",
		Path:        "v2/acme/app/manifests/stable",
		Digest:      "sha256:" + strings.Repeat("d", 64),
		Size:        100,
		ContentType: ociManifestMediaTypeForTest,
		Kind:        "oci-manifest",
		Reference:   "stable",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(context.Background(), domain.Asset{
		Repository:    "oci-private",
		Path:          "v2/acme/app/manifests/" + localReferrerDigest,
		Digest:        localReferrerDigest,
		Size:          200,
		ContentType:   ociManifestMediaTypeForTest,
		Kind:          "oci-manifest",
		Reference:     localReferrerDigest,
		SubjectDigest: subjectDigest,
	}); err != nil {
		t.Fatal(err)
	}

	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(request.URL.Path, "/tags/list"):
				return testHTTPResponse(
					request,
					http.StatusOK,
					`{"name":"acme/app","tags":["latest","stable"]}`,
				), nil
			case strings.Contains(request.URL.Path, "/referrers/"):
				payload := fmt.Sprintf(
					`{"schemaVersion":2,"mediaType":%q,"manifests":[`+
						`{"mediaType":%q,"digest":%q,"size":300}]}`,
					oci.IndexMediaType,
					ociManifestMediaTypeForTest,
					proxyReferrerDigest,
				)
				return testHTTPResponse(request, http.StatusOK, payload), nil
			default:
				t.Fatalf("unexpected OCI catalog path %q", request.URL.Path)
				return nil, nil
			}
		}),
	})

	tags := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci-public/v2/acme/app/tags/list",
		nil,
		true,
	)
	assertStatus(t, tags, http.StatusOK)
	var tagPayload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(tags.Body).Decode(&tagPayload); err != nil {
		t.Fatal(err)
	}
	tags.Body.Close()
	if strings.Join(tagPayload.Tags, ",") != "latest,stable" {
		t.Fatalf("unexpected merged tags: %v", tagPayload.Tags)
	}

	referrers := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci-public/v2/acme/app/referrers/"+subjectDigest,
		nil,
		true,
	)
	assertStatus(t, referrers, http.StatusOK)
	if referrers.Header.Get("Content-Type") != oci.IndexMediaType {
		t.Fatalf("unexpected referrers media type %q", referrers.Header.Get("Content-Type"))
	}
	var referrerPayload oci.Index
	if err := json.NewDecoder(referrers.Body).Decode(&referrerPayload); err != nil {
		t.Fatal(err)
	}
	referrers.Body.Close()
	if len(referrerPayload.Manifests) != 2 {
		t.Fatalf("unexpected merged referrers: %+v", referrerPayload.Manifests)
	}
}

func TestOCIHostedMonolithicPushManifestTagsAndPull(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	layer := []byte("an OCI layer")
	layerDigest := testDigest(layer)
	configuration := []byte("{}")
	configurationDigest := testDigest(configuration)

	configurationPath := "/repository/oci/v2/acme/widget/blobs/uploads/?digest=" +
		configurationDigest
	configurationUpload := fixture.request(
		t,
		http.MethodPost,
		configurationPath,
		configuration,
		true,
	)
	assertStatus(t, configurationUpload, http.StatusCreated)
	configurationUpload.Body.Close()

	uploadPath := "/repository/oci/v2/acme/widget/blobs/uploads/?digest=" + layerDigest
	upload := fixture.request(t, http.MethodPost, uploadPath, layer, true)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	manifestJSON := `{
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json",
            "digest": %q,
            "size": 2
        },
        "layers": []
    }`
	manifestBytes := []byte(fmt.Sprintf(manifestJSON, configurationDigest))
	manifestPath := "/repository/oci/v2/acme/widget/manifests/v1.0.0"
	manifest := fixture.requestWithContentType(
		t,
		http.MethodPut,
		manifestPath,
		manifestBytes,
		"application/vnd.oci.image.manifest.v1+json",
		true,
	)
	assertStatus(t, manifest, http.StatusCreated)
	manifestDigest := manifest.Header.Get("Docker-Content-Digest")
	manifest.Body.Close()
	if manifestDigest != testDigest(manifestBytes) {
		t.Fatalf("got manifest digest %q, want %q", manifestDigest, testDigest(manifestBytes))
	}

	pulledLayer := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci/v2/acme/widget/blobs/"+layerDigest,
		nil,
		true,
	)
	assertStatus(t, pulledLayer, http.StatusOK)
	assertBody(t, pulledLayer, layer)

	pulledManifest := fixture.request(t, http.MethodGet, manifestPath, nil, true)
	assertStatus(t, pulledManifest, http.StatusOK)
	assertBody(t, pulledManifest, manifestBytes)

	tags := fixture.request(
		t,
		http.MethodGet,
		"/repository/oci/v2/acme/widget/tags/list",
		nil,
		true,
	)
	assertStatus(t, tags, http.StatusOK)
	defer tags.Body.Close()
	var tagResponse struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(tags.Body).Decode(&tagResponse); err != nil {
		t.Fatal(err)
	}
	if tagResponse.Name != "acme/widget" || len(tagResponse.Tags) != 1 {
		t.Fatalf("unexpected tag response: %+v", tagResponse)
	}
	if tagResponse.Tags[0] != "v1.0.0" {
		t.Fatalf("got tag %q, want v1.0.0", tagResponse.Tags[0])
	}
}

func TestStandardOCIRootMount(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	unauthenticated := fixture.request(t, http.MethodGet, "/v2/", nil, false)
	assertStatus(t, unauthenticated, http.StatusUnauthorized)
	if unauthenticated.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("unauthenticated GET /v2/ omitted WWW-Authenticate")
	}
	unauthenticated.Body.Close()

	response := fixture.request(t, http.MethodGet, "/v2/", nil, true)
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()

	version := response.Header.Get("Docker-Distribution-API-Version")
	if version != "registry/2.0" {
		t.Fatalf("got distribution API version %q, want registry/2.0", version)
	}
}

func TestOCIResumableBlobUpload(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	start := fixture.request(
		t,
		http.MethodPost,
		"/v2/team/application/blobs/uploads/",
		nil,
		true,
	)
	assertStatus(t, start, http.StatusAccepted)
	uploadLocation := start.Header.Get("Location")
	start.Body.Close()
	if uploadLocation == "" {
		t.Fatal("upload response did not include a Location header")
	}

	firstChunk := fixture.request(
		t,
		http.MethodPatch,
		uploadLocation,
		[]byte("first-"),
		true,
	)
	assertStatus(t, firstChunk, http.StatusAccepted)
	firstChunk.Body.Close()

	content := []byte("first-second")
	completionPath := uploadLocation + "?digest=" + testDigest(content)
	completed := fixture.request(
		t,
		http.MethodPut,
		completionPath,
		[]byte("second"),
		true,
	)
	assertStatus(t, completed, http.StatusCreated)
	completed.Body.Close()

	pulled := fixture.request(
		t,
		http.MethodGet,
		"/v2/team/application/blobs/"+testDigest(content),
		nil,
		true,
	)
	assertStatus(t, pulled, http.StatusOK)
	assertBody(t, pulled, content)
}

func TestHostedUploadsReportPayloadTooLarge(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.MaxUploadBytes = 4 })
	content := []byte("oversized")

	ociResponse := fixture.request(
		t,
		http.MethodPost,
		"/v2/team/application/blobs/uploads/?digest="+testDigest(content),
		content,
		true,
	)
	assertStatus(t, ociResponse, http.StatusRequestEntityTooLarge)
	assertOCIErrorCode(t, ociResponse, "BLOB_UPLOAD_INVALID")

	start := fixture.request(
		t,
		http.MethodPost,
		"/v2/team/application/blobs/uploads/",
		nil,
		true,
	)
	assertStatus(t, start, http.StatusAccepted)
	uploadLocation := start.Header.Get("Location")
	start.Body.Close()
	resumableResponse := fixture.request(
		t,
		http.MethodPut,
		uploadLocation+"?digest="+testDigest(content),
		content,
		true,
	)
	assertStatus(t, resumableResponse, http.StatusRequestEntityTooLarge)
	assertOCIErrorCode(t, resumableResponse, "BLOB_UPLOAD_INVALID")

	rawResponse := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/oversized.bin",
		content,
		true,
	)
	assertStatus(t, rawResponse, http.StatusRequestEntityTooLarge)
	rawResponse.Body.Close()
}

func TestGarbageCollectionDryRunAndApply(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/obsolete/file.bin",
		[]byte("obsolete"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	deleted := fixture.request(
		t,
		http.MethodDelete,
		"/repository/raw/obsolete/file.bin",
		nil,
		true,
	)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()

	dryRun := fixture.request(
		t,
		http.MethodPost,
		"/api/v1/gc?dryRun=true&grace=0s",
		nil,
		true,
	)
	assertStatus(t, dryRun, http.StatusOK)
	var preview garbageCollectionResult
	if err := json.NewDecoder(dryRun.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	dryRun.Body.Close()
	if len(preview.WouldDelete) != 1 || preview.Deleted != 0 {
		t.Fatalf("unexpected garbage collection preview: %+v", preview)
	}

	apply := fixture.request(
		t,
		http.MethodPost,
		"/api/v1/gc?dryRun=false&grace=0s",
		nil,
		true,
	)
	assertStatus(t, apply, http.StatusOK)
	var result garbageCollectionResult
	if err := json.NewDecoder(apply.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	apply.Body.Close()
	if result.Deleted != 1 || result.Reclaimed != int64(len("obsolete")) {
		t.Fatalf("unexpected garbage collection result: %+v", result)
	}
}

func TestClassificationAndCleanupPolicyDryRunAndApply(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	for requestPath, content := range map[string]string{
		"/repository/raw/builds/application-1.0-rc1.zip": "candidate",
		"/repository/raw/builds/application-1.0.zip":     "release",
	} {
		response := fixture.request(
			t,
			http.MethodPut,
			requestPath,
			[]byte(content),
			true,
		)
		assertStatus(t, response, http.StatusCreated)
		response.Body.Close()
	}

	classification := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/classification",
		[]byte(`{
			"rules":[
				{"when":[{"path":"sys.path","op":"matches","value":"-(alpha|beta|rc[0-9]*)\\.zip$"}],"key":"label","value":"prerelease"},
				{"when":[{"path":"sys.path","op":"matches","value":"-[0-9]+\\.[0-9]+\\.zip$"}],"key":"label","value":"release"}
			]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, classification, http.StatusOK)
	classification.Body.Close()

	prerelease, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"builds/application-1.0-rc1.zip",
	)
	if err != nil {
		t.Fatal(err)
	}
	if label := classificationLabel(prerelease.Attributes); label != "prerelease" {
		t.Fatalf("asset classification attributes = %+v", prerelease.Attributes)
	}

	createPolicy := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/cleanup-policies",
		[]byte(`{
			"name":"discard-prereleases",
			"repositories":["raw"],
			"criteria":[{"path":"classification.label","op":"=","value":"prerelease"}],
			"action":"delete",
			"enabled":false
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, createPolicy, http.StatusCreated)
	createPolicy.Body.Close()

	dryRun := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories/raw/cleanup?policy=discard-prereleases&dryRun=true",
		nil,
		"",
		testToken,
	)
	assertStatus(t, dryRun, http.StatusOK)
	var preview domain.Task
	if err := json.NewDecoder(dryRun.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	dryRun.Body.Close()
	if preview.Status != "succeeded" || preview.Result["matched"] != float64(1) {
		t.Fatalf("unexpected cleanup preview task: %+v", preview)
	}
	if _, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"builds/application-1.0-rc1.zip",
	); err != nil {
		t.Fatalf("dry-run deleted the candidate: %v", err)
	}

	apply := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/repositories/raw/cleanup?policy=discard-prereleases&dryRun=false",
		nil,
		"",
		testToken,
	)
	assertStatus(t, apply, http.StatusOK)
	var applied domain.Task
	if err := json.NewDecoder(apply.Body).Decode(&applied); err != nil {
		t.Fatal(err)
	}
	apply.Body.Close()
	if applied.Result["deleted"] != float64(1) {
		t.Fatalf("unexpected applied cleanup task: %+v", applied)
	}
	if _, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"builds/application-1.0-rc1.zip",
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cleanup candidate returned %v, want ErrNotFound", err)
	}
	if _, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"builds/application-1.0.zip",
	); err != nil {
		t.Fatalf("cleanup deleted the release: %v", err)
	}

	tasks := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/tasks",
		nil,
		"",
		testToken,
	)
	assertStatus(t, tasks, http.StatusOK)
	var historyPage httpx.CollectionPage[domain.Task]
	if err := json.NewDecoder(tasks.Body).Decode(&historyPage); err != nil {
		t.Fatal(err)
	}
	tasks.Body.Close()
	history := historyPage.Items
	if len(history) != 2 || history[0].ID != applied.ID {
		t.Fatalf("unexpected cleanup task history: %+v", history)
	}
}

func TestCleanupCandidateCriteriaAndRetention(t *testing.T) {
	now := time.Now().UTC()
	assets := []domain.Asset{
		{
			ID:        1,
			Path:      "example/image/manifests/alpha-1",
			Kind:      "oci-manifest",
			Reference: "alpha-1",
			Attributes: map[string]any{
				"classification": map[string]any{"label": "prerelease"},
				"vulnerability":  map[string]any{"severity": "critical"},
			},
			CreatedAt: now.Add(-90 * 24 * time.Hour),
			UpdatedAt: now.Add(-80 * 24 * time.Hour),
		},
		{
			ID:        2,
			Path:      "example/image/manifests/alpha-2",
			Kind:      "oci-manifest",
			Reference: "alpha-2",
			Attributes: map[string]any{
				"classification": map[string]any{"label": "prerelease"},
				"vulnerability":  map[string]any{"severity": "critical"},
			},
			CreatedAt: now.Add(-70 * 24 * time.Hour),
			UpdatedAt: now.Add(-60 * 24 * time.Hour),
		},
		{
			ID:        3,
			Path:      "example/image/manifests/alpha-3",
			Kind:      "oci-manifest",
			Reference: "alpha-3",
			Attributes: map[string]any{
				"classification": map[string]any{"label": "prerelease"},
				"vulnerability":  map[string]any{"severity": "critical"},
			},
			CreatedAt: now.Add(-50 * 24 * time.Hour),
			UpdatedAt: now.Add(-40 * 24 * time.Hour),
		},
	}
	policy := domain.CleanupPolicy{
		Criteria: domain.CleanupCriteria{
			{Path: "classification.label", Op: "=", Value: "prerelease"},
			{Path: "sys.updatedAt", Op: "before", Value: "30d"},
			{Path: "sys.path", Op: "matches", Value: `alpha-`},
			{Path: "vulnerability.severity", Op: "=", Value: "critical"},
			{Path: "sys.blobStore", Op: "=", Value: "default"},
		},
		KeepLast: 2,
	}
	candidates, err := selectCleanupCandidates(
		policy,
		domain.Repository{Name: "oci", Format: "oci", Type: "hosted", BlobStore: "default"},
		nil,
		assets,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != 1 {
		t.Fatalf("unexpected retained cleanup candidates: %+v", candidates)
	}
}

func TestAssetAPIProjectsAttributesAndRejectsReservedWrites(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/application.zip",
		[]byte("application"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	asset, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"releases/application.zip",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetAttributes(
		context.Background(),
		"raw",
		asset.ID,
		"sys",
		map[string]any{"blobStore": "forged", "path": "forged"},
	); err != nil {
		t.Fatal(err)
	}

	assetResponse := fixture.requestWithBearer(
		t,
		http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/raw/assets/%d", asset.ID),
		nil,
		"",
		testToken,
	)
	assertStatus(t, assetResponse, http.StatusOK)
	responseBody, err := io.ReadAll(assetResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	assetResponse.Body.Close()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &fields); err != nil {
		t.Fatal(err)
	}
	if _, found := fields["classification"]; found {
		t.Fatalf("asset API retained a top-level classification field: %s", responseBody)
	}
	var projected domain.Asset
	if err := json.Unmarshal(responseBody, &projected); err != nil {
		t.Fatal(err)
	}
	system := projected.Attributes["sys"].(map[string]any)
	if system["blobStore"] != "default" || system["path"] != asset.Path {
		t.Fatalf("reserved collision escaped projection: %+v", system)
	}
	raw := projected.Attributes["raw"].(map[string]any)
	if raw["path"] != asset.Path {
		t.Fatalf("raw coordinate projection = %+v", raw)
	}

	for _, namespace := range []string{"classification", "provenance", "sys", "oci", "raw"} {
		response := fixture.requestWithBearer(
			t,
			http.MethodPut,
			fmt.Sprintf(
				"/api/v1/repositories/raw/assets/%d/attributes/%s",
				asset.ID,
				namespace,
			),
			[]byte(`{"value":"forged"}`),
			"application/json",
			testToken,
		)
		assertStatus(t, response, http.StatusForbidden)
		response.Body.Close()
	}
}

func TestScheduledCleanupUsesLeaseAndRecordsTasks(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Cluster = true })
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/nightly/obsolete.zip",
		[]byte("obsolete nightly"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()
	ctx := context.Background()
	if _, err := fixture.Metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository: "raw",
		Rules: []domain.ClassificationRule{
			{
				When:  []domain.Predicate{{Path: "sys.path", Op: "contains", Value: "obsolete"}},
				Key:   "label",
				Value: "ephemeral",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateCleanupPolicy(ctx, domain.CleanupPolicy{
		Name:         "scheduled-ephemeral",
		Repositories: []string{"raw"},
		Criteria: domain.CleanupCriteria{{
			Path: "classification.label", Op: "=", Value: "ephemeral",
		}},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	fixture.Handler.runScheduledCleanup(ctx)
	if _, err := fixture.Metadata.Asset(ctx, "raw", "nightly/obsolete.zip"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("scheduled cleanup returned %v, want ErrNotFound", err)
	}
	tasks, err := fixture.Metadata.Tasks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d scheduled tasks, want cleanup only", len(tasks))
	}
	if tasks[0].Type != "cleanup" {
		t.Fatalf("unexpected scheduled task: %+v", tasks)
	}

	fixture.Handler.runScheduledGarbageCollection(ctx)
	tasks, err = fixture.Metadata.Tasks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Type != "garbage-collection" {
		t.Fatalf("independent garbage-collection task = %+v", tasks)
	}
	leaderResponse := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/tasks/leader",
		nil,
		"",
		testToken,
	)
	assertStatus(t, leaderResponse, http.StatusOK)
	var status schedulerStatus
	if err := json.NewDecoder(leaderResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	leaderResponse.Body.Close()
	if status.Lease == nil || status.Lease.Holder != fixture.Handler.schedulerID {
		t.Fatalf("got scheduler lease %+v, want holder %q", status.Lease, fixture.Handler.schedulerID)
	}
	if status.Intervals.GC == "" {
		t.Fatal("scheduler status did not report the configured GC interval")
	}

	otherHolder, err := fixture.Metadata.AcquireLease(
		ctx,
		cleanupLeaseName,
		"other-node",
		time.Now().UTC(),
		time.Now().UTC().Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if otherHolder {
		t.Fatal("another scheduler stole an active lease")
	}
	gcLease, err := fixture.Metadata.Lease(ctx, gcLeaseName)
	if err != nil {
		t.Fatal(err)
	}
	if gcLease.Holder != fixture.Handler.schedulerID {
		t.Fatalf(
			"garbage-collection lease holder = %q, want %q",
			gcLease.Holder,
			fixture.Handler.schedulerID,
		)
	}
}

func TestGarbageCollectionSchedulerRunsWhenCleanupIsDisabled(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.CleanupInterval = 0 })
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.GCInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.Handler.StartScheduler(ctx)

	// The scheduler runs GC once on start in a goroutine, then persists a task; a
	// tight deadline flakes under loaded CI (Postgres plus parallel suites) even
	// though the run is prompt. Wait generously — the loop returns as soon as the
	// task succeeds, so the bound only bites when GC genuinely never runs.
	deadline := time.Now().Add(30 * time.Second)
	for {
		tasks, err := fixture.Metadata.Tasks(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) > 0 && tasks[0].Status == "succeeded" {
			for _, task := range tasks {
				if task.Type != "garbage-collection" {
					t.Fatalf("cleanup-disabled scheduler task = %+v", task)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("garbage collection did not run while cleanup was disabled")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestOCIManifestCleanupReleasesDependencyGraph(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	layerContent := []byte("unused OCI layer")
	layerDigest := testDigest(layerContent)
	if _, err := fixture.Handler.blobs.Put(
		ctx,
		layerDigest,
		bytes.NewReader(layerContent),
	); err != nil {
		t.Fatal(err)
	}
	manifestContent := []byte(`{"schemaVersion":2}`)
	manifestDigest := testDigest(manifestContent)
	if _, err := fixture.Handler.blobs.Put(
		ctx,
		manifestDigest,
		bytes.NewReader(manifestContent),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci",
		Path:       "v2/example/image/blobs/" + layerDigest,
		Digest:     layerDigest,
		Size:       int64(len(layerContent)),
		Kind:       "oci-blob",
	}); err != nil {
		t.Fatal(err)
	}
	manifest := domain.Asset{
		Repository:   "oci",
		Digest:       manifestDigest,
		Size:         int64(len(manifestContent)),
		Kind:         "oci-manifest",
		Dependencies: []string{layerDigest},
	}
	manifest.Path = "v2/example/image/manifests/nightly"
	manifest.Reference = "nightly"
	if _, err := fixture.Metadata.PutAsset(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Path = "v2/example/image/manifests/" + manifestDigest
	manifest.Reference = manifestDigest
	if _, err := fixture.Metadata.PutAsset(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository: "oci",
		Rules: []domain.ClassificationRule{
			{
				When:  []domain.Predicate{{Path: "oci.tag", Op: "=", Value: "nightly"}},
				Key:   "label",
				Value: "ephemeral",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy := domain.CleanupPolicy{
		Name:         "oci-ephemeral",
		Repositories: []string{"oci"},
		Criteria: domain.CleanupCriteria{{
			Path: "classification.label", Op: "=", Value: "ephemeral",
		}},
	}
	result, err := fixture.Handler.cleanupRepository(ctx, policy, "oci", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.Cascaded != 1 {
		t.Fatalf("unexpected OCI cleanup result: %+v", result)
	}
	if _, err := fixture.Handler.runGarbageCollection(ctx, false, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Handler.blobs.Head(ctx, layerDigest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreferenced OCI layer returned %v, want ErrNotFound", err)
	}
	if _, err := fixture.Handler.blobs.Head(ctx, manifestDigest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreferenced OCI manifest returned %v, want ErrNotFound", err)
	}
}

func TestRepositoryPrivilegesAndTokenScopes(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name: "raw-publisher",
		Privileges: []string{
			"repository:raw:read",
			"repository:raw:write",
			"repository:raw:annotate",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateUser(ctx, "publisher", "publisher-password", false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(ctx, "publisher", []string{"raw-publisher"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.CreateToken(
		ctx,
		"publisher",
		"writer",
		"publisher-token",
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.CreateToken(
		ctx,
		"publisher",
		"read-only",
		"read-token",
		[]string{"repository:raw:read"},
	); err != nil {
		t.Fatal(err)
	}

	upload := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/repository/raw/scoped.bin",
		[]byte("scoped"),
		"application/octet-stream",
		"publisher-token",
	)
	assertStatus(t, upload, http.StatusCreated)
	var asset domain.Asset
	if err := json.NewDecoder(upload.Body).Decode(&asset); err != nil {
		t.Fatal(err)
	}
	upload.Body.Close()

	annotationPath := fmt.Sprintf(
		"/api/v1/repositories/raw/assets/%d/attributes/promotion",
		asset.ID,
	)
	annotation := fixture.requestWithBearer(
		t,
		http.MethodPut,
		annotationPath,
		[]byte(`{"status":"approved"}`),
		"application/json",
		"publisher-token",
	)
	assertStatus(t, annotation, http.StatusCreated)
	annotation.Body.Close()

	forbiddenDelete := fixture.requestWithBearer(
		t,
		http.MethodDelete,
		"/repository/raw/scoped.bin",
		nil,
		"",
		"publisher-token",
	)
	assertStatus(t, forbiddenDelete, http.StatusForbidden)
	forbiddenDelete.Body.Close()

	read := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/repository/raw/scoped.bin",
		nil,
		"",
		"read-token",
	)
	assertStatus(t, read, http.StatusOK)
	assertBody(t, read, []byte("scoped"))

	forbiddenWrite := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/repository/raw/blocked.bin",
		[]byte("blocked"),
		"application/octet-stream",
		"read-token",
	)
	assertStatus(t, forbiddenWrite, http.StatusForbidden)
	forbiddenWrite.Body.Close()
}

func TestOIDCBearerAuthenticationAndProviderAdministration(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name:       "oidc-publisher",
		Privileges: []string{"repository:raw:write"},
	}); err != nil {
		t.Fatal(err)
	}

	createBody := []byte(`{
		"name":"corporate",
		"issuer":"https://identity.example",
		"clientId":"suxen",
		"clientSecret":"server-side-secret",
		"groupRoles":{"release-engineering":["oidc-publisher"]}
	}`)
	created := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/oidc-providers",
		createBody,
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	createdJSON, err := io.ReadAll(created.Body)
	if err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if bytes.Contains(createdJSON, []byte("server-side-secret")) {
		t.Fatal("OIDC provider response exposed the client secret")
	}
	stored, err := fixture.Metadata.OIDCProvider(ctx, "corporate")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientSecret != "server-side-secret" {
		t.Fatal("OIDC provider did not retain its client secret")
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	discoveryRequests := 0
	jwksRequests := 0
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.String() {
			case "https://identity.example/.well-known/openid-configuration":
				discoveryRequests++
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"issuer":                 "https://identity.example",
					"authorization_endpoint": "https://identity.example/authorize",
					"token_endpoint":         "https://identity.example/token",
					"jwks_uri":               "https://identity.example/keys",
				}), nil
			case "https://identity.example/keys":
				jwksRequests++
				return jsonHTTPResponse(request, http.StatusOK, testJWKS(&privateKey.PublicKey)), nil
			default:
				return nil, fmt.Errorf("unexpected OIDC request %s", request.URL)
			}
		}),
	})

	validToken := signTestIDToken(t, privateKey, []string{"release-engineering"})
	firstUpload := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/repository/raw/from-oidc.bin",
		[]byte("OIDC-authenticated content"),
		"application/octet-stream",
		validToken,
	)
	assertStatus(t, firstUpload, http.StatusCreated)
	firstUpload.Body.Close()

	secondUpload := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/repository/raw/from-cached-oidc.bin",
		[]byte("OIDC verifier cache"),
		"application/octet-stream",
		validToken,
	)
	assertStatus(t, secondUpload, http.StatusCreated)
	secondUpload.Body.Close()
	if discoveryRequests != 1 || jwksRequests != 1 {
		t.Fatalf(
			"OIDC verifier was not cached: discovery=%d jwks=%d",
			discoveryRequests,
			jwksRequests,
		)
	}

	unmappedToken := signTestIDToken(t, privateKey, []string{"other-team"})
	forbidden := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/repository/raw/forbidden-oidc.bin",
		[]byte("blocked"),
		"application/octet-stream",
		unmappedToken,
	)
	assertStatus(t, forbidden, http.StatusForbidden)
	forbidden.Body.Close()

	updateBody := []byte(`{
		"issuer":"https://identity.example",
		"clientId":"suxen",
		"defaultRoles":["oidc-publisher"]
	}`)
	updated := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/oidc-providers/corporate",
		updateBody,
		"application/json",
		testToken,
	)
	assertStatus(t, updated, http.StatusOK)
	updated.Body.Close()
	stored, err = fixture.Metadata.OIDCProvider(ctx, "corporate")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientSecret != "server-side-secret" {
		t.Fatal("OIDC update without a clientSecret erased the stored secret")
	}
}

func TestOIDCAuthorizationCodePKCELogin(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) {
		cfg.PublicURL = "https://registry.example"
	})
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{
		Name:       "browser-publisher",
		Privileges: []string{"repository:raw:write"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateOIDCProvider(ctx, domain.OIDCProvider{
		Name:         "browser-idp",
		Issuer:       "https://browser-idp.example",
		ClientID:     "suxen-browser",
		ClientSecret: "oauth-client-secret",
		GroupRoles: map[string][]string{
			"publishers": {"browser-publisher"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	expectedVerifier := ""
	expectedNonce := ""
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.String() {
			case "https://browser-idp.example/.well-known/openid-configuration":
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"issuer":                 "https://browser-idp.example",
					"authorization_endpoint": "https://browser-idp.example/authorize",
					"token_endpoint":         "https://browser-idp.example/token",
					"jwks_uri":               "https://browser-idp.example/keys",
				}), nil
			case "https://browser-idp.example/token":
				if err := request.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if request.Form.Get("code_verifier") != expectedVerifier {
					t.Errorf("token exchange did not send the PKCE verifier")
				}
				if request.Form.Get("code") != "authorization-code" {
					t.Errorf("unexpected authorization code %q", request.Form.Get("code"))
				}
				idToken := signTestIDTokenForIssuer(
					t,
					privateKey,
					"https://browser-idp.example",
					"suxen-browser",
					[]string{"publishers"},
					expectedNonce,
				)
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"access_token": "access-token",
					"token_type":   "Bearer",
					"expires_in":   3600,
					"id_token":     idToken,
				}), nil
			case "https://browser-idp.example/keys":
				return jsonHTTPResponse(request, http.StatusOK, testJWKS(&privateKey.PublicKey)), nil
			default:
				return nil, fmt.Errorf("unexpected OIDC request %s", request.URL)
			}
		}),
	})

	login := fixture.request(
		t,
		http.MethodGet,
		"/auth/oidc/browser-idp/login?redirect=/repository/raw/browser.bin",
		nil,
		false,
	)
	assertStatus(t, login, http.StatusFound)
	loginCookie := responseCookie(t, login, identity.LoginCookieName)
	if !loginCookie.Secure {
		t.Fatal("login transaction cookie is not Secure for HTTPS public URL")
	}
	authorizationURL, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	transaction, err := fixture.Handler.identity.VerifyOIDCLoginTransaction(loginCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	expectedVerifier = transaction.Verifier
	expectedNonce = transaction.Nonce
	challenge := sha256.Sum256([]byte(transaction.Verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(challenge[:])
	if authorizationURL.Query().Get("code_challenge") != expectedChallenge {
		t.Fatal("authorization redirect did not contain the expected PKCE challenge")
	}
	if authorizationURL.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("authorization redirect did not request the S256 PKCE method")
	}

	callbackPath := "/auth/oidc/browser-idp/callback?code=authorization-code&state=" +
		url.QueryEscape(transaction.State)
	callbackRequest, err := http.NewRequest(http.MethodGet, callbackPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	callbackRequest.AddCookie(loginCookie)
	callbackRecorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(callbackRecorder, callbackRequest)
	callback := callbackRecorder.Result()
	assertStatus(t, callback, http.StatusFound)
	if callback.Header.Get("Location") != "/repository/raw/browser.bin" {
		t.Fatalf("unexpected post-login redirect %q", callback.Header.Get("Location"))
	}
	sessionCookie := responseCookie(t, callback, identity.SessionCookieName)
	if !sessionCookie.Secure {
		t.Fatal("OIDC session cookie is not Secure for HTTPS public URL")
	}
	clearedLoginCookie := responseDeletedCookie(t, callback, identity.LoginCookieName)
	if !clearedLoginCookie.Secure || clearedLoginCookie.MaxAge != -1 {
		t.Fatalf("cleared login cookie = %+v, want Secure deletion", clearedLoginCookie)
	}
	callback.Body.Close()

	uploadRequest, err := http.NewRequest(
		http.MethodPut,
		"/repository/raw/browser.bin",
		strings.NewReader("browser session"),
	)
	if err != nil {
		t.Fatal(err)
	}
	uploadRequest.AddCookie(sessionCookie)
	uploadRequest.Host = "registry.example"
	uploadRequest.Header.Set("Origin", "https://registry.example")
	uploadRecorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(uploadRecorder, uploadRequest)
	upload := uploadRecorder.Result()
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	logoutRequest, err := http.NewRequest(http.MethodPost, "/auth/oidc/browser-idp/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logoutRequest.Host = "registry.example"
	logoutRequest.Header.Set("Origin", "https://registry.example")
	logoutRequest.AddCookie(sessionCookie)
	logoutRecorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(logoutRecorder, logoutRequest)
	logout := logoutRecorder.Result()
	assertStatus(t, logout, http.StatusNoContent)
	clearedSessionCookie := responseDeletedCookie(t, logout, identity.SessionCookieName)
	if !clearedSessionCookie.Secure || clearedSessionCookie.MaxAge != -1 {
		t.Fatalf("cleared session cookie = %+v, want Secure deletion", clearedSessionCookie)
	}
	logout.Body.Close()
}

func TestSilentOIDCRefresh(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateOIDCProvider(ctx, domain.OIDCProvider{
		Name:         "browser-idp",
		Issuer:       "https://browser-idp.example",
		ClientID:     "suxen-browser",
		ClientSecret: "oauth-client-secret",
	}); err != nil {
		t.Fatal(err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	expectedNonce := ""
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.String() {
			case "https://browser-idp.example/.well-known/openid-configuration":
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"issuer":                 "https://browser-idp.example",
					"authorization_endpoint": "https://browser-idp.example/authorize",
					"token_endpoint":         "https://browser-idp.example/token",
					"jwks_uri":               "https://browser-idp.example/keys",
				}), nil
			case "https://browser-idp.example/token":
				idToken := signTestIDTokenForIssuer(
					t, privateKey, "https://browser-idp.example", "suxen-browser", nil, expectedNonce,
				)
				return jsonHTTPResponse(request, http.StatusOK, map[string]any{
					"access_token": "access-token",
					"token_type":   "Bearer",
					"expires_in":   3600,
					"id_token":     idToken,
				}), nil
			case "https://browser-idp.example/keys":
				return jsonHTTPResponse(request, http.StatusOK, testJWKS(&privateKey.PublicKey)), nil
			default:
				return nil, fmt.Errorf("unexpected OIDC request %s", request.URL)
			}
		}),
	})

	// A prompt=none login forwards prompt=none to the IdP and marks the
	// transaction silent.
	login := fixture.request(
		t, http.MethodGet,
		"/auth/oidc/browser-idp/login?prompt=none&redirect=/ui/%23/repositories", nil, false,
	)
	assertStatus(t, login, http.StatusFound)
	loginCookie := responseCookie(t, login, identity.LoginCookieName)
	authorizationURL, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if authorizationURL.Query().Get("prompt") != "none" {
		t.Fatalf("silent login authorize URL prompt = %q, want none", authorizationURL.Query().Get("prompt"))
	}
	transaction, err := fixture.Handler.identity.VerifyOIDCLoginTransaction(loginCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if !transaction.Silent {
		t.Fatal("prompt=none login did not mark the transaction silent")
	}
	expectedNonce = transaction.Nonce

	// A silent success sets the session cookie and lands back on the requested route.
	successCallback := oidcCallbackResult(t, fixture,
		"/auth/oidc/browser-idp/callback?code=authorization-code&state="+url.QueryEscape(transaction.State),
		loginCookie,
	)
	assertStatus(t, successCallback, http.StatusFound)
	if location := successCallback.Header.Get("Location"); location != "/ui/#/repositories" {
		t.Fatalf("silent success redirect = %q, want /ui/#/repositories", location)
	}
	sessionCookie := responseCookie(t, successCallback, identity.SessionCookieName)
	successCallback.Body.Close()

	// whoami on that session reports the provider and expiry so the UI can schedule
	// the next silent refresh.
	whoamiRequest, err := http.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	whoamiRequest.AddCookie(sessionCookie)
	whoamiRecorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(whoamiRecorder, whoamiRequest)
	whoami := whoamiRecorder.Result()
	assertStatus(t, whoami, http.StatusOK)
	var who identityResponse
	if err := json.NewDecoder(whoami.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	whoami.Body.Close()
	if who.AuthenticationKind != "oidc-session" {
		t.Fatalf("whoami authenticationKind = %q, want oidc-session", who.AuthenticationKind)
	}
	if who.SessionProvider != "browser-idp" {
		t.Fatalf("whoami sessionProvider = %q, want browser-idp", who.SessionProvider)
	}
	if who.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("whoami expiresAt = %d, want a future unix time", who.ExpiresAt)
	}

	// A silent attempt the IdP cannot satisfy without interaction (login_required)
	// returns the user to the app, not an error page — the still-valid session keeps
	// working and the UI falls back to a visible login only once it truly lapses.
	silentLogin := fixture.request(
		t, http.MethodGet,
		"/auth/oidc/browser-idp/login?prompt=none&redirect=/ui/%23/overview", nil, false,
	)
	assertStatus(t, silentLogin, http.StatusFound)
	silentCookie := responseCookie(t, silentLogin, identity.LoginCookieName)
	silentLogin.Body.Close()
	silentTransaction, err := fixture.Handler.identity.VerifyOIDCLoginTransaction(silentCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	silentFailure := oidcCallbackResult(t, fixture,
		"/auth/oidc/browser-idp/callback?error=login_required&state="+url.QueryEscape(silentTransaction.State),
		silentCookie,
	)
	assertStatus(t, silentFailure, http.StatusFound)
	if location := silentFailure.Header.Get("Location"); location != "/ui/#/overview" {
		t.Fatalf("silent login_required redirect = %q, want /ui/#/overview", location)
	}
	silentFailure.Body.Close()

	// A non-silent login_required is still a visible error, not a redirect.
	visibleLogin := fixture.request(t, http.MethodGet, "/auth/oidc/browser-idp/login?redirect=/ui/", nil, false)
	assertStatus(t, visibleLogin, http.StatusFound)
	visibleCookie := responseCookie(t, visibleLogin, identity.LoginCookieName)
	visibleLogin.Body.Close()
	visibleTransaction, err := fixture.Handler.identity.VerifyOIDCLoginTransaction(visibleCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	visibleFailure := oidcCallbackResult(t, fixture,
		"/auth/oidc/browser-idp/callback?error=login_required&state="+url.QueryEscape(visibleTransaction.State),
		visibleCookie,
	)
	assertStatus(t, visibleFailure, http.StatusUnauthorized)
	visibleFailure.Body.Close()
}

func oidcCallbackResult(
	t *testing.T,
	fixture *serverFixture,
	path string,
	loginCookie *http.Cookie,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(loginCookie)
	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestNamedWebhookPutIsIdempotentAndPathAuthoritative(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const webhookSecret = "named-webhook-api-secret"

	createdResponse := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/webhooks/scanner",
		[]byte(`{
			"name":"ignored-body-name",
			"url":"https://scanner.example/hooks/initial",
			"secret":"named-webhook-api-secret",
			"events":["asset.uploaded"],
			"repositories":["raw"]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, createdResponse, http.StatusCreated)
	if location := createdResponse.Header.Get("Location"); location != "/api/v1/webhooks/scanner" {
		t.Fatalf("created webhook Location = %q", location)
	}
	createdBody, err := io.ReadAll(createdResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	createdResponse.Body.Close()
	if bytes.Contains(createdBody, []byte(webhookSecret)) {
		t.Fatal("named webhook response exposed its signing secret")
	}
	if bytes.Contains(createdBody, []byte(`"id"`)) {
		t.Fatal("named webhook response exposed a removed numeric identifier")
	}
	var created domain.Webhook
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "scanner" {
		t.Fatalf("named webhook path was not authoritative: %+v", created)
	}

	updatedResponse := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/webhooks/scanner",
		[]byte(`{
			"name":"another-ignored-name",
			"url":"https://scanner.example/hooks/updated",
			"events":["asset.deleted"],
			"repositories":["raw"],
			"enabled":false
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, updatedResponse, http.StatusOK)
	var updated domain.Webhook
	if err := json.NewDecoder(updatedResponse.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	updatedResponse.Body.Close()
	if updated.Name != created.Name || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("repeated named PUT changed webhook identity: created=%+v updated=%+v", created, updated)
	}
	if updated.Name != "scanner" || updated.URL != "https://scanner.example/hooks/updated" || updated.Enabled {
		t.Fatalf("repeated named PUT did not replace webhook fields: %+v", updated)
	}
	stored, err := fixture.Metadata.Webhook(context.Background(), "scanner")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Secret != webhookSecret {
		t.Fatal("repeated named PUT replaced an omitted signing secret")
	}

	itemResponse := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/webhooks/scanner",
		nil,
		"",
		testToken,
	)
	assertStatus(t, itemResponse, http.StatusOK)
	itemResponse.Body.Close()
	webhooks, err := fixture.Metadata.Webhooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(webhooks) != 1 {
		t.Fatalf("repeated named PUT created %d webhooks, want 1", len(webhooks))
	}

	missingSecret := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/webhooks/missing-secret",
		[]byte(`{
			"url":"https://scanner.example/hooks/missing",
			"events":["asset.uploaded"]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, missingSecret, http.StatusBadRequest)
	missingSecret.Body.Close()
}

func TestSignedWebhookDeliveryAndAttributeDownloadGate(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const webhookSecret = "test-webhook-signing-secret"

	var receivedBody []byte
	var receivedSignature string
	var receivedEvent string
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var err error
			receivedBody, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			receivedSignature = request.Header.Get("X-Suxen-Signature-256")
			receivedEvent = request.Header.Get("X-Suxen-Event")
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		}),
	})

	createWebhookBody := fmt.Sprintf(`{
		"name":"scanner",
		"url":%q,
		"secret":%q,
		"events":["asset.uploaded"],
		"repositories":["raw"]
	}`, "https://scanner.example/webhooks/suxen", webhookSecret)
	createWebhook := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/webhooks",
		[]byte(createWebhookBody),
		"application/json",
		testToken,
	)
	assertStatus(t, createWebhook, http.StatusCreated)
	createdJSON, err := io.ReadAll(createWebhook.Body)
	if err != nil {
		t.Fatal(err)
	}
	createWebhook.Body.Close()
	if bytes.Contains(createdJSON, []byte(webhookSecret)) {
		t.Fatal("webhook API exposed its signing secret")
	}
	var webhook domain.Webhook
	if err := json.Unmarshal(createdJSON, &webhook); err != nil {
		t.Fatal(err)
	}

	content := []byte("quarantined artifact")
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/quarantine.bin",
		content,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	delivered, err := fixture.Handler.content.DeliverWebhookBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("delivered %d webhooks, want 1", delivered)
	}
	if receivedEvent != domain.WebhookAssetUploaded {
		t.Fatalf("received event %q", receivedEvent)
	}
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	_, _ = mac.Write(receivedBody)
	expectedSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(receivedSignature), []byte(expectedSignature)) {
		t.Fatalf("invalid webhook signature %q", receivedSignature)
	}
	var event domain.WebhookEvent
	if err := json.Unmarshal(receivedBody, &event); err != nil {
		t.Fatal(err)
	}
	if event.Asset == nil || event.Asset.Path != "quarantine.bin" {
		t.Fatalf("unexpected webhook event: %+v", event)
	}
	if system, ok := event.Asset.Attributes["sys"].(map[string]any); !ok || system["blobStore"] != "default" {
		t.Fatalf("webhook asset attributes were not projected: %+v", event.Asset.Attributes)
	}

	deliveryHistory := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/api/v1/webhooks/scanner/deliveries",
		nil,
		"",
		testToken,
	)
	assertStatus(t, deliveryHistory, http.StatusOK)
	var deliveriesPage httpx.CollectionPage[domain.WebhookDelivery]
	if err := json.NewDecoder(deliveryHistory.Body).Decode(&deliveriesPage); err != nil {
		t.Fatal(err)
	}
	deliveryHistory.Body.Close()
	deliveries := deliveriesPage.Items
	if len(deliveries) != 1 || deliveries[0].Status != "delivered" {
		t.Fatalf("unexpected webhook delivery history: %+v", deliveries)
	}
	if deliveries[0].WebhookName != webhook.Name {
		t.Fatalf("delivery webhook name = %q, want %q", deliveries[0].WebhookName, webhook.Name)
	}

	gate := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/download-gate",
		[]byte(`{
			"criteria":[{"path":"scan.status","op":"=","value":"passed"}]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, gate, http.StatusCreated)
	if location := gate.Header.Get("Location"); location != "/api/v1/repositories/raw/download-gate" {
		t.Fatalf("created gate Location = %q", location)
	}
	gate.Body.Close()

	blocked := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/quarantine.bin",
		nil,
		true,
	)
	assertStatus(t, blocked, http.StatusForbidden)
	blocked.Body.Close()

	asset, err := fixture.Metadata.Asset(
		context.Background(),
		"raw",
		"quarantine.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	attributes := fixture.requestWithBearer(
		t,
		http.MethodPut,
		fmt.Sprintf(
			"/api/v1/repositories/raw/assets/%d/attributes/scan",
			asset.ID,
		),
		[]byte(`{"status":"passed"}`),
		"application/json",
		testToken,
	)
	assertStatus(t, attributes, http.StatusCreated)
	attributes.Body.Close()

	released := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/quarantine.bin",
		nil,
		true,
	)
	assertStatus(t, released, http.StatusOK)
	assertBody(t, released, content)

	systemGate := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/download-gate",
		[]byte(`{
			"criteria":[{"path":"sys.blobStore","op":"=","value":"default"}]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, systemGate, http.StatusOK)
	systemGate.Body.Close()

	projectedGateAllowed := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/quarantine.bin",
		nil,
		true,
	)
	assertStatus(t, projectedGateAllowed, http.StatusOK)
	projectedGateAllowed.Body.Close()

	restoreScanGate := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/download-gate",
		[]byte(`{
			"criteria":[{"path":"scan.status","op":"=","value":"passed"}]
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, restoreScanGate, http.StatusOK)
	restoreScanGate.Body.Close()

	replacement := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/quarantine.bin",
		[]byte("replacement requiring a new scan"),
		true,
	)
	assertStatus(t, replacement, http.StatusCreated)
	replacement.Body.Close()
	requarantined := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/quarantine.bin",
		nil,
		true,
	)
	assertStatus(t, requarantined, http.StatusForbidden)
	requarantined.Body.Close()
}

func TestDownloadGateInstanceDefaultsMerge(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	content := []byte("artifact under instance defaults")
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/thing.bin",
		content,
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	get := func() int {
		response := fixture.request(t, http.MethodGet, "/repository/raw/thing.bin", nil, true)
		response.Body.Close()
		return response.StatusCode
	}

	// No gate anywhere: the asset downloads.
	if status := get(); status != http.StatusOK {
		t.Fatalf("baseline download status = %d, want 200", status)
	}

	// An instance-wide default gates every inheriting repository.
	setDefaults := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/download-gate-defaults",
		[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}`),
		"application/json",
		testToken,
	)
	assertStatus(t, setDefaults, http.StatusCreated)
	if location := setDefaults.Header.Get("Location"); location != "/api/v1/download-gate-defaults" {
		t.Fatalf("created default Location = %q", location)
	}
	setDefaults.Body.Close()
	if status := get(); status != http.StatusForbidden {
		t.Fatalf("inherited default download status = %d, want 403", status)
	}

	readDefaults := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/download-gate-defaults", nil, "", testToken,
	)
	assertStatus(t, readDefaults, http.StatusOK)
	var stored domain.DownloadGate
	if err := json.NewDecoder(readDefaults.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	readDefaults.Body.Close()
	if len(stored.Criteria) != 1 || stored.Criteria[0].Value != "passed" {
		t.Fatalf("stored default = %+v", stored)
	}

	// A repository opts out with an empty-criteria gate: it downloads again.
	optOut := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/download-gate",
		[]byte(`{"criteria":[],"inheritGlobal":false}`),
		"application/json",
		testToken,
	)
	assertStatus(t, optOut, http.StatusCreated)
	optOut.Body.Close()
	if status := get(); status != http.StatusOK {
		t.Fatalf("opted-out download status = %d, want 200", status)
	}

	// Re-inheriting AND-extends: the default's criterion is added back to the
	// repository's own, so the still-missing scan attribute blocks the download.
	inheriting := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/download-gate",
		[]byte(`{"criteria":[{"path":"sys.blobStore","op":"=","value":"default"}],"inheritGlobal":true}`),
		"application/json",
		testToken,
	)
	assertStatus(t, inheriting, http.StatusOK)
	inheriting.Body.Close()
	if status := get(); status != http.StatusForbidden {
		t.Fatalf("AND-extended download status = %d, want 403", status)
	}

	// Removing the default leaves only the repository's own matching criterion.
	deleteDefaults := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/download-gate-defaults", nil, "", testToken,
	)
	assertStatus(t, deleteDefaults, http.StatusNoContent)
	deleteDefaults.Body.Close()
	if status := get(); status != http.StatusOK {
		t.Fatalf("repository-only download status = %d, want 200", status)
	}
}

func TestClassificationInstanceDefaultsMerge(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	upload := fixture.request(t, http.MethodPut, "/repository/raw/thing.bin", []byte("x"), true)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	labelOf := func() string {
		asset, err := fixture.Metadata.Asset(context.Background(), "raw", "thing.bin")
		if err != nil {
			t.Fatal(err)
		}
		return classificationLabel(asset.Attributes)
	}
	if label := labelOf(); label != "" {
		t.Fatalf("baseline label = %q, want empty", label)
	}

	// The instance-wide default retroactively labels inheriting repositories.
	setDefaults := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/classification-defaults",
		[]byte(`{"rules":[{"when":[],"key":"label","value":"public"}]}`),
		"application/json",
		testToken,
	)
	assertStatus(t, setDefaults, http.StatusCreated)
	if location := setDefaults.Header.Get("Location"); location != "/api/v1/classification-defaults" {
		t.Fatalf("created default Location = %q", location)
	}
	setDefaults.Body.Close()
	if label := labelOf(); label != "public" {
		t.Fatalf("inherited default label = %q, want public", label)
	}

	// A repository rule overrides the inherited default on the same key.
	override := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/classification",
		[]byte(`{"rules":[{"when":[],"key":"label","value":"team"}],"inheritGlobal":true}`),
		"application/json",
		testToken,
	)
	assertStatus(t, override, http.StatusOK)
	override.Body.Close()
	if label := labelOf(); label != "team" {
		t.Fatalf("overridden label = %q, want team", label)
	}

	// Opting out with no repository rules drops the label entirely.
	optOut := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/repositories/raw/classification",
		[]byte(`{"rules":[],"inheritGlobal":false}`),
		"application/json",
		testToken,
	)
	assertStatus(t, optOut, http.StatusOK)
	optOut.Body.Close()
	if label := labelOf(); label != "" {
		t.Fatalf("opted-out label = %q, want empty", label)
	}

	deleteDefaults := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/classification-defaults", nil, "", testToken,
	)
	assertStatus(t, deleteDefaults, http.StatusNoContent)
	deleteDefaults.Body.Close()
}

func TestClassificationDefaultsRequireAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/classification-defaults", nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	if challenge := response.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("control-plane 401 advertised %q", challenge)
	}
	response.Body.Close()
}

func TestTrustPolicyInstanceDefaultsLifecycle(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	missing := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/trust-policy-defaults", nil, "", testToken,
	)
	assertStatus(t, missing, http.StatusNotFound)
	missing.Body.Close()

	_, publicKey := provenanceTestKey(t)
	body, err := json.Marshal(map[string]any{
		"mode":       "audit",
		"publicKeys": []string{publicKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/trust-policy-defaults",
		body,
		"application/json",
		testToken,
	)
	assertStatus(t, created, http.StatusCreated)
	if location := created.Header.Get("Location"); location != "/api/v1/trust-policy-defaults" {
		t.Fatalf("created default Location = %q", location)
	}
	created.Body.Close()

	read := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/trust-policy-defaults", nil, "", testToken,
	)
	assertStatus(t, read, http.StatusOK)
	var stored domain.TrustPolicy
	if err := json.NewDecoder(read.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if stored.Mode != "audit" || stored.Repository != "" {
		t.Fatalf("stored default = %+v", stored)
	}

	// An invalid default (no trust material) is rejected.
	invalid := fixture.requestWithBearer(
		t,
		http.MethodPut,
		"/api/v1/trust-policy-defaults",
		[]byte(`{"mode":"audit"}`),
		"application/json",
		testToken,
	)
	assertStatus(t, invalid, http.StatusBadRequest)
	invalid.Body.Close()

	deleted := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/trust-policy-defaults", nil, "", testToken,
	)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
}

func TestTrustPolicyDefaultsRequireAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/trust-policy-defaults", nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	if challenge := response.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("control-plane 401 advertised %q", challenge)
	}
	response.Body.Close()
}

func TestDownloadGateDefaultsRequireAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/download-gate-defaults", nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	if challenge := response.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("control-plane 401 advertised %q", challenge)
	}
	response.Body.Close()
}

// TestInstanceDefaultsRejectManagedMutation confirms that once a default is owned
// by declarative provisioning, the runtime API refuses to edit or delete it. The
// managed guard runs before any body validation, so the 409 does not depend on a
// valid request body.
func TestInstanceDefaultsRejectManagedMutation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	cases := []struct {
		path string
		kind string
	}{
		{"/api/v1/download-gate-defaults", "downloadGate"},
		{"/api/v1/classification-defaults", "classification"},
		{"/api/v1/trust-policy-defaults", "trustPolicy"},
	}
	for _, tc := range cases {
		if err := fixture.Metadata.PutProvisionRecord(ctx, store.ProvisionRecord{
			Kind: tc.kind, Name: domain.InstanceDefaultsName, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		put := fixture.requestWithBearer(
			t, http.MethodPut, tc.path, []byte(`{}`), "application/json", testToken,
		)
		assertStatus(t, put, http.StatusConflict)
		put.Body.Close()
		del := fixture.requestWithBearer(t, http.MethodDelete, tc.path, nil, "", testToken)
		assertStatus(t, del, http.StatusConflict)
		del.Body.Close()
	}
}

// TestDownloadGateDefaultsManagedForceRelease confirms that GET reports managed
// state and that a forced mutation overrides the provisioned default and releases
// declarative ownership, after which the default is editable again.
func TestDownloadGateDefaultsManagedForceRelease(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()

	create := fixture.requestWithBearer(
		t, http.MethodPut, "/api/v1/download-gate-defaults",
		[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}`),
		"application/json", testToken,
	)
	assertStatus(t, create, http.StatusCreated)
	create.Body.Close()
	if err := fixture.Metadata.PutProvisionRecord(ctx, store.ProvisionRecord{
		Kind: "downloadGate", Name: domain.InstanceDefaultsName, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	get := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/download-gate-defaults", nil, "", testToken,
	)
	assertStatus(t, get, http.StatusOK)
	var managed domain.DownloadGate
	if err := json.NewDecoder(get.Body).Decode(&managed); err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if !managed.Managed {
		t.Fatalf("GET should report managed=true for a provisioned default: %+v", managed)
	}

	force := fixture.requestWithBearer(
		t, http.MethodPut, "/api/v1/download-gate-defaults?force=true",
		[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"blocked"}]}`),
		"application/json", testToken,
	)
	assertStatus(t, force, http.StatusOK)
	force.Body.Close()
	if _, err := fixture.Metadata.ProvisionRecord(ctx, "downloadGate", domain.InstanceDefaultsName); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("forced edit should release provisioning ownership: %v", err)
	}

	after := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/download-gate-defaults", nil, "", testToken,
	)
	assertStatus(t, after, http.StatusOK)
	var released domain.DownloadGate
	if err := json.NewDecoder(after.Body).Decode(&released); err != nil {
		t.Fatal(err)
	}
	after.Body.Close()
	if released.Managed {
		t.Fatalf("GET should report managed=false after force-release: %+v", released)
	}
	del := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/download-gate-defaults", nil, "", testToken,
	)
	assertStatus(t, del, http.StatusNoContent)
	del.Body.Close()
}

func newServerFixture(t *testing.T) *serverFixture {
	return newServerFixtureWithAccess(t, false)
}

func newServerFixtureWithAnonymousRead(t *testing.T) *serverFixture {
	return newServerFixtureWithAccess(t, true)
}

func newServerFixtureWithAccess(t *testing.T, anonymousRead bool) *serverFixture {
	t.Helper()
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = metadata.Close()
	})
	blobPath := filepath.Join(dataDirectory, "blobs")
	blobStore, err := blob.NewFS(blobPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		DataDir:           dataDirectory,
		BlobURL:           "fs://" + blobPath,
		BootstrapUser:     "admin",
		BootstrapPassword: "test-password",
		BootstrapToken:    testToken,
		OIDCStateSecret:   "test-only-oidc-state-secret-32-bytes",
		MaxUploadBytes:    16 << 20,
		ProxyManifestTTL:  5 * time.Minute,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(cfg, metadata, blobStore, logger)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if anonymousRead {
		anonymous, err := metadata.Role(context.Background(), "anonymous")
		if err != nil {
			t.Fatal(err)
		}
		anonymous.Privileges = []string{"repository:*:read"}
		if err := metadata.UpdateRole(context.Background(), anonymous); err != nil {
			t.Fatal(err)
		}
	}
	// Drain detached best-effort goroutines (e.g. async last-download updates)
	// before metadata.Close and t.TempDir removal, so their writes cannot race
	// cleanup ("directory not empty"). Registered last so it runs first (LIFO).
	t.Cleanup(func() {
		_ = handler.Close()
	})

	return &serverFixture{
		Metadata: metadata,
		Handler:  handler,
	}
}

func TestFreshServerRequiresAuthenticationForArtifactReads(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/private/release.tar.gz",
		nil,
		false,
	)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()

	anonymous, err := fixture.Metadata.Role(context.Background(), "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if len(anonymous.Privileges) != 0 {
		t.Fatalf("fresh server grants anonymous privileges: %v", anonymous.Privileges)
	}
}

func (fixture *serverFixture) request(
	t *testing.T,
	method string,
	requestPath string,
	body []byte,
	authenticated bool,
) *http.Response {
	t.Helper()
	return fixture.requestWithContentType(
		t,
		method,
		requestPath,
		body,
		"application/octet-stream",
		authenticated,
	)
}

func (fixture *serverFixture) requestWithContentType(
	t *testing.T,
	method string,
	requestPath string,
	body []byte,
	contentType string,
	authenticated bool,
) *http.Response {
	t.Helper()
	token := ""
	if authenticated {
		token = testToken
	}
	return fixture.requestWithBearer(
		t,
		method,
		requestPath,
		body,
		contentType,
		token,
	)
}

func (fixture *serverFixture) requestWithBearer(
	t *testing.T,
	method string,
	requestPath string,
	body []byte,
	contentType string,
	token string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	// Attribute writers are generation-bound. Supply the current digest for
	// routine fixture mutations; dedicated precondition tests build requests
	// directly when exercising rejection behavior.
	if (method == http.MethodPut || method == http.MethodDelete) &&
		strings.Contains(requestPath, "/attributes/") {
		parts := strings.Split(strings.Trim(requestPath, "/"), "/")
		if len(parts) >= 8 {
			assetID, parseErr := strconv.ParseInt(parts[5], 10, 64)
			if parseErr == nil {
				asset, lookupErr := fixture.Metadata.AssetByID(context.Background(), parts[3], assetID)
				if lookupErr == nil {
					request.Header.Set("If-Match", content.QuoteETag(asset.Digest))
				}
			}
		}
	}

	recorder := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestArtifactResponsesAreSandboxedDownloads(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upload := fixture.requestWithContentType(
		t, http.MethodPut, "/repository/raw/page.html",
		[]byte(`<script>document.body.textContent = document.domain</script>`),
		"text/html", true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	download := fixture.request(t, http.MethodGet, "/repository/raw/page.html", nil, true)
	assertStatus(t, download, http.StatusOK)
	defer download.Body.Close()
	if download.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("Content-Disposition = %q, want attachment", download.Header.Get("Content-Disposition"))
	}
	if !strings.Contains(download.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", download.Header.Get("Content-Security-Policy"))
	}
	if download.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", download.Header.Get("X-Content-Type-Options"))
	}
}

func TestAssetAttributeWritesRequireCurrentGeneration(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upload := fixture.request(
		t, http.MethodPut, "/repository/raw/precondition.bin", []byte("generation"), true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	asset, err := fixture.Metadata.Asset(context.Background(), "raw", "precondition.bin")
	if err != nil {
		t.Fatal(err)
	}
	requestPath := fmt.Sprintf(
		"/api/v1/repositories/raw/assets/%d/attributes/scanner", asset.ID,
	)
	request := func(ifMatch ...string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(
			http.MethodPut, requestPath, strings.NewReader(`{"status":"passed"}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		for _, value := range ifMatch {
			req.Header.Add("If-Match", value)
		}
		recorder := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(recorder, req)
		return recorder.Result()
	}

	missing := request()
	assertStatus(t, missing, http.StatusPreconditionRequired)
	missing.Body.Close()

	stale := request(content.QuoteETag(strings.Repeat("0", 64)))
	assertStatus(t, stale, http.StatusPreconditionFailed)
	stale.Body.Close()

	for _, invalid := range []string{"*", "W/" + content.QuoteETag(asset.Digest), content.QuoteETag(asset.Digest) + `, nonsense`} {
		response := request(invalid)
		assertStatus(t, response, http.StatusPreconditionFailed)
		response.Body.Close()
	}

	oldTag := content.QuoteETag(strings.Repeat("0", 64))
	currentTag := content.QuoteETag(asset.Digest)
	current := request(oldTag, currentTag)
	assertStatus(t, current, http.StatusCreated)
	current.Body.Close()
	for _, values := range [][]string{
		{oldTag + ", " + currentTag},
		{currentTag},
		{asset.Digest},
	} {
		response := request(values...)
		assertStatus(t, response, http.StatusOK)
		response.Body.Close()
	}
}

func assertStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	if response.StatusCode == expected {
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	t.Fatalf("got status %d, want %d; body: %s", response.StatusCode, expected, body)
}

func assertBody(t *testing.T, response *http.Response, expected []byte) {
	t.Helper()
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("got body %q, want %q", actual, expected)
	}
}

func testDigest(content []byte) string {
	hash := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(hash[:])
}

const ociManifestMediaTypeForTest = "application/vnd.oci.image.manifest.v1+json"

func testHTTPResponse(
	request *http.Request,
	status int,
	body string,
) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func jsonHTTPResponse(
	request *http.Request,
	status int,
	value any,
) *http.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	response := testHTTPResponse(request, status, string(body))
	response.Header.Set("Content-Type", "application/json")
	return response
}

func testJWKS(publicKey *rsa.PublicKey) map[string]any {
	exponent := []byte{
		byte(publicKey.E >> 16),
		byte(publicKey.E >> 8),
		byte(publicKey.E),
	}
	return map[string]any{
		"keys": []map[string]string{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": "test-key",
				"n":   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(exponent),
			},
		},
	}
}

func signTestIDToken(t *testing.T, privateKey *rsa.PrivateKey, groups []string) string {
	return signTestIDTokenForIssuer(
		t,
		privateKey,
		"https://identity.example",
		"suxen",
		groups,
		"",
	)
}

func signTestIDTokenForIssuer(
	t *testing.T,
	privateKey *rsa.PrivateKey,
	issuer string,
	audience string,
	groups []string,
	nonce string,
) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"kid": "test-key",
		"typ": "JWT",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claimsMap := map[string]any{
		"iss":                issuer,
		"sub":                "user-123",
		"aud":                audience,
		"iat":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"preferred_username": "release-bot",
		"groups":             groups,
	}
	if nonce != "" {
		claimsMap["nonce"] = nonce
	}
	claims, err := json.Marshal(claimsMap)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func responseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.MaxAge >= 0 {
			return cookie
		}
	}
	t.Fatalf("response did not contain cookie %q", name)
	return nil
}

func responseDeletedCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.MaxAge < 0 {
			return cookie
		}
	}
	t.Fatalf("response did not delete cookie %q", name)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestControlPlane401OmitsBasicChallenge(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	// Control-plane routes must not send WWW-Authenticate: Basic, so browsers do
	// not raise the native Basic-auth popup on the SPA's XHRs; the SPA redirects
	// to OIDC on the bare 401 instead.
	control := fixture.request(t, http.MethodGet, "/api/v1/blob-stores", nil, false)
	assertStatus(t, control, http.StatusUnauthorized)
	if challenge := control.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("control-plane 401 sent WWW-Authenticate %q, want none", challenge)
	}
	control.Body.Close()

	// whoami with rejected credentials is a control-plane route too and must stay
	// challenge-free.
	whoami := fixture.requestWithBearer(
		t, http.MethodGet, "/api/v1/whoami", nil, "application/octet-stream", "not-a-valid-token",
	)
	assertStatus(t, whoami, http.StatusUnauthorized)
	if challenge := whoami.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("whoami 401 sent WWW-Authenticate %q, want none", challenge)
	}
	whoami.Body.Close()

	// Registry routes keep the challenge because Docker/OCI clients require it.
	registry := fixture.request(t, http.MethodPut, "/repository/raw/releases/x.tar.gz", []byte("x"), false)
	assertStatus(t, registry, http.StatusUnauthorized)
	if registry.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("registry 401 omitted WWW-Authenticate")
	}
	registry.Body.Close()
}

func TestRunCleanupPolicyAcrossRepositories(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	put := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/builds/app-1.0.zip",
		[]byte("release"),
		true,
	)
	assertStatus(t, put, http.StatusCreated)
	put.Body.Close()

	createPolicy := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/cleanup-policies",
		[]byte(`{
			"name":"sweep-raw",
			"repositories":["raw"],
			"criteria":[{"path":"sys.path","op":"exists"}],
			"keepLast":0,
			"action":"delete",
			"enabled":false
		}`),
		"application/json",
		testToken,
	)
	assertStatus(t, createPolicy, http.StatusCreated)
	createPolicy.Body.Close()

	run := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/api/v1/cleanup-policies/sweep-raw/run?dryRun=true",
		nil,
		"",
		testToken,
	)
	assertStatus(t, run, http.StatusOK)
	var result struct {
		Policy string        `json:"policy"`
		DryRun bool          `json:"dryRun"`
		Tasks  []domain.Task `json:"tasks"`
	}
	if err := json.NewDecoder(run.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	run.Body.Close()
	if result.Policy != "sweep-raw" || !result.DryRun {
		t.Fatalf("unexpected run result envelope: %+v", result)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("expected one task per attached repository, got %d", len(result.Tasks))
	}
	task := result.Tasks[0]
	if task.Status != "succeeded" || task.Repository != "raw" || !task.DryRun {
		t.Fatalf("unexpected cleanup task: %+v", task)
	}
}

func classificationLabel(attributes map[string]any) string {
	classification, _ := attributes["classification"].(map[string]any)
	label, _ := classification["label"].(string)
	return label
}
