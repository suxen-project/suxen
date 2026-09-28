package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/provision"
	spiblob "github.com/suxen-project/suxen/spi/blob"
)

func TestProvisionExistingBlobStoreComparesBeforeOpening(t *testing.T) {
	fixture := newServerFixture(t)
	configuration := "fs://" + t.TempDir()
	t.Setenv("SUXEN_PROVISION_COMPARE_STORE", configuration)
	t.Setenv("SUXEN_PROVISION_CHANGED_STORE", "fs://"+t.TempDir())
	opens := 0
	fixture.Handler.blobStores.Factory = func(driver, value string) (spiblob.Store, error) {
		if value == configuration {
			opens++
		}
		return blob.NewFS(strings.TrimPrefix(value, "fs://"))
	}
	created := blobStoreRequest(t, fixture.Handler, http.MethodPost, "/api/v1/blob-stores",
		`{"name":"compared","driver":"fs","configurationRef":{"env":"SUXEN_PROVISION_COMPARE_STORE"}}`)
	if created.Code != http.StatusCreated || opens != 1 {
		t.Fatalf("create = %d, opens=%d: %s", created.Code, opens, created.Body.String())
	}
	apply := func(spec string, dryRun bool) provision.Report {
		t.Helper()
		path := "/api/v1/provision?dryRun=false"
		if dryRun {
			path = "/api/v1/provision?dryRun=true"
		}
		response := blobStoreRequest(t, fixture.Handler, http.MethodPost, path,
			`{"apiVersion":"suxen.io/v1","resources":[{"kind":"blobStore","name":"compared","spec":`+spec+`}]}`)
		if response.Code != http.StatusOK {
			t.Fatalf("provision = %d: %s", response.Code, response.Body.String())
		}
		var report provision.Report
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	base := `{"driver":"fs","configurationRef":{"env":"SUXEN_PROVISION_COMPARE_STORE"}`
	for _, test := range []struct {
		spec string
		dry  bool
		want string
	}{
		{base + `}`, true, provision.StatusUnchanged},
		{base + `}`, false, provision.StatusUnchanged},
		{base + `,"attributes":{"note":"safe"}}`, true, provision.StatusUpdated},
		{base + `,"attributes":{"note":"safe"}}`, false, provision.StatusUpdated},
	} {
		report := apply(test.spec, test.dry)
		if report.Failed() || !reportHasBlobStoreStatus(report, "compared", test.want) || opens != 1 {
			t.Fatalf("reconcile dryRun=%v want=%s opens=%d: %+v", test.dry, test.want, opens, report)
		}
	}
	changed := apply(`{"driver":"fs","configurationRef":{"env":"SUXEN_PROVISION_CHANGED_STORE"}}`, false)
	if !changed.Failed() || opens != 1 {
		t.Fatalf("immutable change opened backend or was accepted: opens=%d report=%+v", opens, changed)
	}
	immutable := false
	for _, result := range changed.Results {
		if result.Kind == "blobStore" && result.Name == "compared" &&
			strings.Contains(result.Error, domain.ErrBlobStoreDefinitionImmutable.Error()) {
			immutable = true
		}
	}
	if !immutable {
		t.Fatalf("immutable change report = %+v", changed)
	}
}

func reportHasBlobStoreStatus(report provision.Report, name, status string) bool {
	for _, result := range report.Results {
		if result.Kind == "blobStore" && result.Name == name && result.Status == status {
			return true
		}
	}
	return false
}
