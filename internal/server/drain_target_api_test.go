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

func TestActiveDrainTargetCannotBeDeletedThroughAPI(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	t.Setenv("SUXEN_PASS6_TARGET", "tracking://"+t.TempDir())
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores",
		`{"name":"target","driver":"tracking","configurationRef":{"env":"SUXEN_PASS6_TARGET"}}`); response.Code != http.StatusCreated {
		t.Fatalf("create target: %d %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain",
		`{"target":"target"}`); response.Code != http.StatusOK {
		t.Fatalf("begin drain: %d %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodDelete, "/api/v1/blob-stores/target", ""); response.Code != http.StatusConflict {
		t.Fatalf("delete active destination: %d %s", response.Code, response.Body.String())
	}
	upload := ociConcurrencyRequest(h, http.MethodPost, "/repository/images/v2/app/blobs/uploads/", "", "")
	if upload.Code != http.StatusAccepted {
		t.Fatalf("upload after rejected deletion: %d %s", upload.Code, upload.Body.String())
	}
	location := upload.Header().Get("Location")
	if response := ociConcurrencyRequest(h, http.MethodDelete, location, "", ""); response.Code != http.StatusNoContent {
		t.Fatalf("cancel upload: %d %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodDelete, "/api/v1/blob-stores/secondary/drain", ""); response.Code != http.StatusOK {
		t.Fatalf("cancel drain: %d %s", response.Code, response.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodDelete, "/api/v1/blob-stores/target", ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete after cancellation: %d %s", response.Code, response.Body.String())
	}
}

func TestProvisionPruneCannotDeleteActiveDrainTarget(t *testing.T) {
	h := newOCIConcurrencyServer(t, true)
	t.Setenv("SUXEN_PASS6_MANAGED_TARGET", "tracking://"+t.TempDir())
	create := blobStoreRequest(t, h, http.MethodPost, "/api/v1/provision?dryRun=false",
		`{"apiVersion":"suxen.io/v1","resources":[{"kind":"blobStore","name":"managed-target","spec":{"driver":"tracking","configurationRef":{"env":"SUXEN_PASS6_MANAGED_TARGET"}}}]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("provision target: %d %s", create.Code, create.Body.String())
	}
	if response := blobStoreRequest(t, h, http.MethodPost, "/api/v1/blob-stores/secondary/drain",
		`{"target":"managed-target"}`); response.Code != http.StatusOK {
		t.Fatalf("begin drain: %d %s", response.Code, response.Body.String())
	}
	prune := blobStoreRequest(t, h, http.MethodPost, "/api/v1/provision?dryRun=false&prune=true",
		`{"apiVersion":"suxen.io/v1","resources":[]}`)
	if prune.Code != http.StatusOK {
		t.Fatalf("prune response: %d %s", prune.Code, prune.Body.String())
	}
	var report provision.Report
	if err := json.Unmarshal(prune.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatalf("prune unexpectedly removed active destination: %+v", report)
	}
	guarded := false
	for _, result := range report.Results {
		if result.Kind == "blobStore" && result.Name == "managed-target" &&
			result.Status == provision.StatusFailed && strings.Contains(result.Error, domain.ErrActiveDrainTarget.Error()) {
			guarded = true
		}
	}
	if !guarded {
		t.Fatalf("prune did not report the shared persistence guard: %+v", report)
	}
	if _, err := h.metadata.BlobStore(context.Background(), "managed-target"); err != nil {
		t.Fatalf("prune removed active destination: %v", err)
	}
	if source, err := h.metadata.BlobStore(context.Background(), "secondary"); err != nil ||
		source.State != domain.BlobStoreStateDraining || source.DrainTarget != "managed-target" {
		t.Fatalf("source after rejected prune = %+v, %v", source, err)
	}
}
