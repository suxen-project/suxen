package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/provision"
)

// Provisioning enforces the same immutable-definition contract as the API: a
// reconcile that changes a blob store's driver, configuration reference or
// physical destination reports the immutable-field conflict instead of
// repointing the backend, while an attribute-only reconcile still applies.
func TestProvisionRejectsBlobStoreDefinitionChange(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	t.Setenv("SUXEN_IMMUTABLE_ORIGINAL", "tracking://"+t.TempDir())
	t.Setenv("SUXEN_IMMUTABLE_REPLACEMENT", "tracking://"+t.TempDir())

	create := blobStoreRequest(t, h, http.MethodPost, "/api/v1/provision?dryRun=false",
		`{"apiVersion":"suxen.io/v1","resources":[{"kind":"blobStore","name":"provisioned","spec":{"driver":"tracking","configurationRef":{"env":"SUXEN_IMMUTABLE_ORIGINAL"}}}]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("provision create: %d %s", create.Code, create.Body.String())
	}

	// An attribute-only reconcile is applied.
	attributes := blobStoreRequest(t, h, http.MethodPost, "/api/v1/provision?dryRun=false",
		`{"apiVersion":"suxen.io/v1","resources":[{"kind":"blobStore","name":"provisioned","spec":{"driver":"tracking","configurationRef":{"env":"SUXEN_IMMUTABLE_ORIGINAL"},"attributes":{"note":"safe"}}}]}`)
	if attributes.Code != http.StatusOK {
		t.Fatalf("provision attribute reconcile: %d %s", attributes.Code, attributes.Body.String())
	}
	var attributeReport provision.Report
	if err := json.Unmarshal(attributes.Body.Bytes(), &attributeReport); err != nil {
		t.Fatal(err)
	}
	if attributeReport.Failed() {
		t.Fatalf("attribute-only reconcile failed: %+v", attributeReport)
	}

	// Changing the configuration reference is rejected as an immutable-field
	// conflict; the stored definition is untouched.
	change := blobStoreRequest(t, h, http.MethodPost, "/api/v1/provision?dryRun=false",
		`{"apiVersion":"suxen.io/v1","resources":[{"kind":"blobStore","name":"provisioned","spec":{"driver":"tracking","configurationRef":{"env":"SUXEN_IMMUTABLE_REPLACEMENT"}}}]}`)
	if change.Code != http.StatusOK {
		t.Fatalf("provision change response: %d %s", change.Code, change.Body.String())
	}
	var changeReport provision.Report
	if err := json.Unmarshal(change.Body.Bytes(), &changeReport); err != nil {
		t.Fatal(err)
	}
	rejected := false
	for _, result := range changeReport.Results {
		if result.Kind == "blobStore" && result.Name == "provisioned" &&
			result.Status == provision.StatusFailed &&
			strings.Contains(result.Error, domain.ErrBlobStoreDefinitionImmutable.Error()) {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("provision did not reject the definition change: %+v", changeReport)
	}
	stored, err := h.metadata.BlobStore(context.Background(), "provisioned")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConfigurationRef == nil || stored.ConfigurationRef.Env != "SUXEN_IMMUTABLE_ORIGINAL" {
		t.Fatalf("definition changed through provisioning: %+v", stored)
	}
}
