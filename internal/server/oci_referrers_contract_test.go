package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/oci"
)

func TestOCIHostedReferrerDescriptorAndSubjectAcknowledgment(t *testing.T) {
	fixture := newServerFixture(t)
	const image = "acme/referrer-contract"
	const manifestType = "application/vnd.oci.image.manifest.v1+json"
	const artifactType = "application/vnd.example.signature.v1+json"
	base := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	response := fixture.requestWithContentType(t, http.MethodPut,
		"/v2/"+image+"/manifests/base", base, "application/vnd.oci.image.index.v1+json", true)
	assertStatus(t, response, http.StatusCreated)
	subjectDigest := response.Header.Get("Docker-Content-Digest")
	response.Body.Close()

	config := []byte(`{}`)
	configDigest := testDigest(config)
	response = fixture.request(t, http.MethodPost,
		"/v2/"+image+"/blobs/uploads/?digest="+configDigest, config, true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()

	referrer := []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":%q,
		"config":{"mediaType":%q,"digest":%q,"size":2},
		"layers":[],
		"subject":{"mediaType":"application/vnd.oci.image.index.v1+json","digest":%q,"size":%d},
		"annotations":{"org.example.note":"signed"}
	}`, manifestType, artifactType, configDigest, subjectDigest, len(base)))
	response = fixture.requestWithContentType(t, http.MethodPut,
		"/v2/"+image+"/manifests/signature", referrer, manifestType, true)
	assertStatus(t, response, http.StatusCreated)
	referrerDigest := response.Header.Get("Docker-Content-Digest")
	if got := response.Header.Get("OCI-Subject"); got != subjectDigest {
		t.Fatalf("OCI-Subject = %q, want %q", got, subjectDigest)
	}
	response.Body.Close()

	response = fixture.request(t, http.MethodGet,
		"/v2/"+image+"/referrers/"+subjectDigest+"?artifactType="+url.QueryEscape(artifactType), nil, true)
	assertStatus(t, response, http.StatusOK)
	if got := response.Header.Get("OCI-Filters-Applied"); got != "artifactType" {
		t.Fatalf("OCI-Filters-Applied = %q", got)
	}
	var index oci.Index
	if err := json.NewDecoder(response.Body).Decode(&index); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(index.Manifests) != 1 {
		t.Fatalf("filtered referrers = %+v", index.Manifests)
	}
	got := index.Manifests[0]
	if got.Digest != referrerDigest || got.ArtifactType != artifactType ||
		got.Annotations["org.example.note"] != "signed" {
		t.Fatalf("referrer descriptor = %+v", got)
	}
}

func TestOCIReferrersRejectInvalidSubjectDigest(t *testing.T) {
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet,
		"/v2/acme/referrer-contract/referrers/sha256:short", nil, true)
	assertStatus(t, response, http.StatusBadRequest)
	assertOCIErrorCode(t, response, "DIGEST_INVALID")
}
