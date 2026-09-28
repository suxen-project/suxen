package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func newDrainTestHandler(t *testing.T) *Server {
	t.Helper()
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUXEN_TEST_SECONDARY_STORE", "tracking://"+filepath.Join(dataDirectory, "secondary"))
	factory := func(driver string, configuration string) (blob.Store, error) {
		return blob.NewFS(strings.TrimPrefix(configuration, "tracking://"))
	}
	handler := NewWithBlobStoreFactory(
		config.Config{
			DataDir:           dataDirectory,
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "default"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
			MaxUploadBytes:    16 << 20,
		},
		metadata,
		defaultStore,
		factory,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestBlobStoreDrainLifecycle(t *testing.T) {
	handler := newDrainTestHandler(t)

	create := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary",
        "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create store = %d: %s", create.Code, create.Body.String())
	}

	// The default store cannot be drained.
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/default/drain", `{"target": "secondary"}`); got.Code != http.StatusConflict {
		t.Fatalf("drain default = %d: %s", got.Code, got.Body.String())
	}
	// A missing or self drain target is rejected.
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "missing"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("drain to missing target = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "secondary"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("drain to self = %d", got.Code)
	}

	drain := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "default"}`)
	if drain.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", drain.Code, drain.Body.String())
	}
	var draining domain.BlobStore
	if err := json.Unmarshal(drain.Body.Bytes(), &draining); err != nil {
		t.Fatal(err)
	}
	if draining.State != domain.BlobStoreStateDraining || draining.DrainTarget != "default" {
		t.Fatalf("state=%q target=%q, want draining/default", draining.State, draining.DrainTarget)
	}

	// A draining store accepts no new bindings, and cannot be re-drained.
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "on-draining", "format": "raw", "type": "hosted", "blobStore": "secondary"
    }`); got.Code != http.StatusConflict {
		t.Fatalf("bind to draining store = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "default"}`); got.Code != http.StatusConflict {
		t.Fatalf("re-drain = %d", got.Code)
	}

	// Clearing the drain returns the store to active.
	clear := blobStoreRequest(t, handler, http.MethodDelete, "/api/v1/blob-stores/secondary/drain", "")
	if clear.Code != http.StatusOK {
		t.Fatalf("clear drain = %d: %s", clear.Code, clear.Body.String())
	}
	var active domain.BlobStore
	if err := json.Unmarshal(clear.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.State != domain.BlobStoreStateActive || active.DrainTarget != "" {
		t.Fatalf("after clear state=%q target=%q", active.State, active.DrainTarget)
	}

	// Binding to the reactivated store now succeeds.
	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "on-active", "format": "raw", "type": "hosted", "blobStore": "secondary"
    }`); got.Code != http.StatusCreated {
		t.Fatalf("bind after clear = %d: %s", got.Code, got.Body.String())
	}
}

// Deleting a store that is the active drain target of another store must be
// refused with a conflict rather than the previous 204, which left the draining
// source writing to a store that no longer existed. Clearing the drain releases
// the target for deletion.
func TestDeletingActiveDrainTargetIsRejected(t *testing.T) {
	handler := newDrainTestHandler(t)
	t.Setenv("SUXEN_TEST_TARGET_STORE", "tracking://"+filepath.Join(t.TempDir(), "target"))

	for _, spec := range []struct{ name, env string }{
		{"secondary", "SUXEN_TEST_SECONDARY_STORE"},
		{"target", "SUXEN_TEST_TARGET_STORE"},
	} {
		got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", fmt.Sprintf(`{
            "name": %q, "driver": "tracking", "configurationRef": {"env": %q}
        }`, spec.name, spec.env))
		if got.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", spec.name, got.Code, got.Body.String())
		}
	}

	if got := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores/secondary/drain", `{"target": "target"}`); got.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", got.Code, got.Body.String())
	}

	if del := blobStoreRequest(t, handler, http.MethodDelete, "/api/v1/blob-stores/target", ""); del.Code != http.StatusConflict {
		t.Fatalf("delete active drain target = %d, want 409: %s", del.Code, del.Body.String())
	}

	if got := blobStoreRequest(t, handler, http.MethodDelete, "/api/v1/blob-stores/secondary/drain", ""); got.Code != http.StatusOK {
		t.Fatalf("clear drain = %d: %s", got.Code, got.Body.String())
	}
	if got := blobStoreRequest(t, handler, http.MethodDelete, "/api/v1/blob-stores/target", ""); got.Code != http.StatusNoContent {
		t.Fatalf("delete released target = %d, want 204: %s", got.Code, got.Body.String())
	}
}
