package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// The instance-default tables no longer store a managed flag; ownership is
// projected from the provisioning record alone. An API-created default reads
// back unmanaged, and provisioning the same default flips it to managed without
// any managed column being written.
func TestDefaultManagedProjectionDerivesFromOwnership(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	created := fixture.requestWithContentType(
		t,
		http.MethodPut,
		"/api/v1/download-gate-defaults",
		[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}`),
		"application/json",
		true,
	)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	if managed := readDownloadGateDefaultManaged(t, fixture); managed {
		t.Fatal("API-created download-gate default is reported managed")
	}

	applied := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		[]byte(`{"apiVersion":"suxen.io/v1","resources":[{"kind":"downloadGate","name":"default","spec":{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}}]}`),
		"application/json",
		true,
	)
	assertStatus(t, applied, http.StatusOK)
	applied.Body.Close()

	if managed := readDownloadGateDefaultManaged(t, fixture); !managed {
		t.Fatal("provisioned download-gate default is not reported managed")
	}
}

func readDownloadGateDefaultManaged(t *testing.T, fixture *serverFixture) bool {
	t.Helper()
	read := fixture.request(t, http.MethodGet, "/api/v1/download-gate-defaults", nil, true)
	assertStatus(t, read, http.StatusOK)
	defer read.Body.Close()
	var gate domain.DownloadGate
	if err := json.NewDecoder(read.Body).Decode(&gate); err != nil {
		t.Fatal(err)
	}
	return gate.Managed
}
