package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// A publisher needs a digest read to attach an OCI signature, but that read
// must still satisfy the repository's independent quarantine gate.
func TestOCISigningReadHonorsDownloadGate(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, principal := range []struct {
		name       string
		privileges []string
	}{
		{name: "publisher", privileges: []string{"repository:oci:read", "repository:oci:write"}},
		{name: "reader", privileges: []string{"repository:oci:read"}},
	} {
		if err := fixture.Metadata.CreateRole(ctx, domain.Role{Name: principal.name, Privileges: principal.privileges}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.Metadata.CreateUser(ctx, principal.name, "password", false); err != nil {
			t.Fatal(err)
		}
		if err := fixture.Metadata.SetUserRoles(ctx, principal.name, []string{principal.name}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.Metadata.CreateToken(ctx, principal.name, "api", principal.name+"-signing-gate-token", nil); err != nil {
			t.Fatal(err)
		}
	}

	config := []byte(`{}`)
	upload := fixture.request(t, http.MethodPost, "/v2/acme/app/blobs/uploads/?digest="+testDigest(config), config, true)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},"layers":[]}`, testDigest(config)))
	upload = fixture.requestWithContentType(t, http.MethodPut, "/v2/acme/app/manifests/latest", manifest, "application/vnd.oci.image.manifest.v1+json", true)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()
	digest := testDigest(manifest)
	paths := []struct {
		name string
		path string
	}{
		{name: "digest", path: "/v2/acme/app/manifests/" + digest},
		{name: "tag", path: "/v2/acme/app/manifests/latest"},
	}
	_, publicKey := provenanceTestKey(t)
	if err := fixture.Metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
		Repository: "oci", Mode: "verify-on-pull", PublicKeys: []string{publicKey},
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := fixture.Metadata.EffectiveTrustPolicy(ctx, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetDownloadGate(ctx, domain.DownloadGate{
		Repository: "oci", Enabled: true,
		Criteria: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, provenancePassed := range []bool{false, true} {
		for _, gatePassed := range []bool{false, true} {
			for _, path := range paths {
				asset, err := fixture.Metadata.Asset(ctx, "oci", path.path[1:])
				if err != nil {
					t.Fatal(err)
				}
				status := "failed"
				if provenancePassed {
					status = "passed"
				}
				if err := fixture.Metadata.SetAttributes(ctx, "oci", asset.ID, "provenance", map[string]any{
					"status": status, "digest": asset.Digest,
					"policyUpdatedAt": policy.UpdatedAt.Format(time.RFC3339Nano),
					"verifiedAt":      time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
				}); err != nil {
					t.Fatal(err)
				}
				scanStatus := "failed"
				if gatePassed {
					scanStatus = "passed"
				}
				if err := fixture.Metadata.SetAttributes(ctx, "oci", asset.ID, "scan", map[string]any{"status": scanStatus}); err != nil {
					t.Fatal(err)
				}
				for _, principal := range []string{"publisher", "reader"} {
					name := fmt.Sprintf("%s/provenance=%t/gate=%t/%s", path.name, provenancePassed, gatePassed, principal)
					t.Run(name, func(t *testing.T) {
						response := fixture.requestWithBearer(t, http.MethodGet, path.path, nil, "", principal+"-signing-gate-token")
						defer response.Body.Close()
						want := http.StatusForbidden
						if gatePassed && (provenancePassed || principal == "publisher" && path.name == "digest") {
							want = http.StatusOK
						}
						if response.StatusCode != want {
							t.Fatalf("GET %s: status=%d, want %d", path.path, response.StatusCode, want)
						}
					})
				}
			}
		}
	}
}
