package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/oci"
)

func TestOCIUploadLedgerBindsEveryOperationToItsOwnerAndImage(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	const secondToken = "second-uploader-token"
	createUploadTestUser(t, fixture, "second-uploader", secondToken)

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

	requests := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{
			name:   "status by another principal",
			method: http.MethodGet,
			path:   uploadLocation,
		},
		{
			name:   "append by another principal",
			method: http.MethodPatch,
			path:   uploadLocation,
			body:   []byte("foreign"),
		},
		{
			name:   "completion by another principal",
			method: http.MethodPut,
			path:   uploadLocation + "?digest=" + testDigest(nil),
		},
		{
			name:   "cancellation by another principal",
			method: http.MethodDelete,
			path:   uploadLocation,
		},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.requestWithBearer(
				t,
				test.method,
				test.path,
				test.body,
				"application/octet-stream",
				secondToken,
			)
			assertStatus(t, response, http.StatusNotFound)
			assertOCIErrorCode(t, response, "BLOB_UPLOAD_UNKNOWN")
		})
	}

	wrongImageLocation := strings.Replace(
		uploadLocation,
		"/team/application/",
		"/team/another-image/",
		1,
	)
	wrongImage := fixture.request(t, http.MethodGet, wrongImageLocation, nil, true)
	assertStatus(t, wrongImage, http.StatusNotFound)
	assertOCIErrorCode(t, wrongImage, "BLOB_UPLOAD_UNKNOWN")

	ownerStatus := fixture.request(t, http.MethodGet, uploadLocation, nil, true)
	assertStatus(t, ownerStatus, http.StatusNoContent)
	ownerStatus.Body.Close()
}

func TestOCIUploadLedgerAppliesFiniteDefaultQuotas(t *testing.T) {
	t.Parallel()
	t.Run("session count", func(t *testing.T) {
		fixture := newServerFixture(t)
		for index := 0; index < int(oci.DefaultUploadPrincipalSessions); index++ {
			response := fixture.request(
				t,
				http.MethodPost,
				"/v2/team/application/blobs/uploads/",
				nil,
				true,
			)
			assertStatus(t, response, http.StatusAccepted)
			response.Body.Close()
		}
		rejected := fixture.request(
			t,
			http.MethodPost,
			"/v2/team/application/blobs/uploads/",
			nil,
			true,
		)
		assertStatus(t, rejected, http.StatusTooManyRequests)
		assertOCIErrorCode(t, rejected, "TOOMANYREQUESTS")
	})

	t.Run("aggregate bytes and per-session bytes", func(t *testing.T) {
		fixture := newServerFixture(t)
		fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.MaxUploadBytes = 10 })
		const secondToken = "quota-test-uploader-token"
		createUploadTestUser(t, fixture, "quota-test-uploader", secondToken)

		firstLocation := startOCIUploadForTest(t, fixture, testToken)
		firstChunk := fixture.requestWithBearer(
			t,
			http.MethodPatch,
			firstLocation,
			[]byte("123456"),
			"application/octet-stream",
			testToken,
		)
		assertStatus(t, firstChunk, http.StatusAccepted)
		firstChunk.Body.Close()

		secondLocation := startOCIUploadForTest(t, fixture, testToken)
		aggregateLimit := fixture.requestWithBearer(
			t,
			http.MethodPatch,
			secondLocation,
			[]byte("12345"),
			"application/octet-stream",
			testToken,
		)
		assertStatus(t, aggregateLimit, http.StatusTooManyRequests)
		assertOCIErrorCode(t, aggregateLimit, "TOOMANYREQUESTS")

		perSessionLimit := fixture.requestWithBearer(
			t,
			http.MethodPatch,
			firstLocation,
			[]byte("12345"),
			"application/octet-stream",
			testToken,
		)
		assertStatus(t, perSessionLimit, http.StatusRequestEntityTooLarge)
		assertOCIErrorCode(t, perSessionLimit, "BLOB_UPLOAD_INVALID")

		foreignLocation := startOCIUploadForTest(t, fixture, secondToken)
		storeLimit := fixture.requestWithBearer(
			t,
			http.MethodPatch,
			foreignLocation,
			[]byte("12345"),
			"application/octet-stream",
			secondToken,
		)
		assertStatus(t, storeLimit, http.StatusTooManyRequests)
		assertOCIErrorCode(t, storeLimit, "TOOMANYREQUESTS")
	})
}

func startOCIUploadForTest(t *testing.T, fixture *serverFixture, token string) string {
	t.Helper()
	response := fixture.requestWithBearer(
		t,
		http.MethodPost,
		"/v2/team/application/blobs/uploads/",
		nil,
		"application/octet-stream",
		token,
	)
	assertStatus(t, response, http.StatusAccepted)
	location := response.Header.Get("Location")
	response.Body.Close()
	if location == "" {
		t.Fatal("upload response did not include a Location header")
	}
	return location
}

func createUploadTestUser(
	t *testing.T,
	fixture *serverFixture,
	username string,
	token string,
) {
	t.Helper()
	ctx := context.Background()
	if err := fixture.Metadata.CreateUser(ctx, username, "test-password", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.CreateToken(
		ctx,
		username,
		"upload-test",
		token,
		[]string{"repository:oci:read", "repository:oci:write", "repository:oci:delete"},
	); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectionReapsStaleUploadSessions(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	var locations []string
	for index := 0; index < int(oci.DefaultUploadPrincipalSessions); index++ {
		locations = append(locations, startOCIUploadForTest(t, fixture, testToken))
	}
	chunk := fixture.requestWithBearer(
		t,
		http.MethodPatch,
		locations[0],
		[]byte("abandoned"),
		"application/octet-stream",
		testToken,
	)
	assertStatus(t, chunk, http.StatusAccepted)
	chunk.Body.Close()
	rejected := fixture.request(t, http.MethodPost, "/v2/team/application/blobs/uploads/", nil, true)
	assertStatus(t, rejected, http.StatusTooManyRequests)
	assertOCIErrorCode(t, rejected, "TOOMANYREQUESTS")

	fresh, err := fixture.Handler.collectGarbage(ctx, false, defaultGCGracePeriod, time.Now().UTC(), "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.StaleUploads != 0 || fresh.StaleUploadsDeleted != 0 {
		t.Fatalf("fresh sessions were reported stale: %+v", fresh)
	}

	later := time.Now().UTC().Add(7 * time.Hour)
	preview, err := fixture.Handler.collectGarbage(ctx, true, defaultGCGracePeriod, later, "")
	if err != nil {
		t.Fatal(err)
	}
	if preview.StaleUploads != 4 || preview.StaleUploadsDeleted != 0 {
		t.Fatalf("unexpected dry-run result: %+v", preview)
	}
	stillRejected := fixture.request(t, http.MethodPost, "/v2/team/application/blobs/uploads/", nil, true)
	assertStatus(t, stillRejected, http.StatusTooManyRequests)
	stillRejected.Body.Close()

	applied, err := fixture.Handler.collectGarbage(ctx, false, defaultGCGracePeriod, later, "")
	if err != nil {
		t.Fatal(err)
	}
	if applied.StaleUploads != 4 || applied.StaleUploadsDeleted != 4 {
		t.Fatalf("unexpected apply result: %+v", applied)
	}
	gone := fixture.request(t, http.MethodGet, locations[0], nil, true)
	assertStatus(t, gone, http.StatusNotFound)
	assertOCIErrorCode(t, gone, "BLOB_UPLOAD_UNKNOWN")
	backing, err := fixture.Handler.blobStores.Store(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	uploadID := locations[0][strings.LastIndex(locations[0], "/")+1:]
	_, _, err = backing.(blob.UploadStore).OpenUpload(ctx, ociUploadStorageKeyForTest("team", uploadID))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("staged upload object survived reaping: %v", err)
	}
	accepted := fixture.request(t, http.MethodPost, "/v2/team/application/blobs/uploads/", nil, true)
	assertStatus(t, accepted, http.StatusAccepted)
	accepted.Body.Close()
}

func ociUploadStorageKeyForTest(repositoryName string, uploadID string) string {
	repositoryHash := sha256.Sum256([]byte(repositoryName))
	return fmt.Sprintf("oci/%x/%s", repositoryHash, uploadID)
}
