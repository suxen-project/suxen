package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBlobStoreUsageReportsPhysicalOccupancy(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	first := fixture.request(t, http.MethodPut, "/repository/raw/a.bin", []byte("aaaa"), true)
	assertStatus(t, first, http.StatusCreated)
	first.Body.Close()
	second := fixture.request(t, http.MethodPut, "/repository/raw/b.bin", []byte("bbbbbb"), true)
	assertStatus(t, second, http.StatusCreated)
	second.Body.Close()

	response := fixture.request(t, http.MethodGet, "/api/v1/blob-stores/default/usage", nil, true)
	assertStatus(t, response, http.StatusOK)
	var usage blobStoreUsage
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if usage.BlobStore != "default" {
		t.Fatalf("blobStore = %q, want default", usage.BlobStore)
	}
	// Two distinct payloads deduplicate to two blobs, both referenced.
	if usage.ObjectCount != 2 || usage.ReferencedCount != 2 || usage.UnreferencedCount != 0 {
		t.Fatalf("unexpected counts: %+v", usage)
	}
	if usage.TotalBytes != 10 || usage.ReferencedBytes != 10 || usage.UnreferencedBytes != 0 {
		t.Fatalf("unexpected bytes: %+v", usage)
	}
}

func TestBlobStoreUsageUnknownStoreNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/blob-stores/missing/usage", nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
}

func TestBlobStoreUsageRequiresAuthentication(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/blob-stores/default/usage", nil, false)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()
}
