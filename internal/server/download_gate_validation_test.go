package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestDownloadGateRequiredCriteriaPreservesQuarantine(t *testing.T) {
	t.Parallel()
	for _, scope := range []struct {
		name string
		path string
	}{
		{"raw", "/api/v1/repositories/raw/download-gate"},
		{"default", "/api/v1/download-gate-defaults"},
	} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/managed=%t", scope.name, managed), func(t *testing.T) {
				f := newServerFixture(t)
				const criteria = `[{"path":"scan.status","op":"=","value":"passed"}]`
				if managed {
					body := fmt.Sprintf(`{"apiVersion":"suxen.io/v1","resources":[{"kind":"downloadGate","name":%q,"spec":{"criteria":%s,"enabled":true}}]}`, scope.name, criteria)
					response := f.requestWithContentType(t, http.MethodPost, "/api/v1/provision", []byte(body), "application/json", true)
					assertStatus(t, response, http.StatusOK)
					response.Body.Close()
				} else {
					response := f.request(t, http.MethodPut, scope.path, []byte(`{"criteria":`+criteria+`}`), true)
					assertStatus(t, response, http.StatusCreated)
					response.Body.Close()
				}
				upload := f.request(t, http.MethodPut, "/repository/raw/unscanned", []byte("unscanned"), true)
				assertStatus(t, upload, http.StatusCreated)
				upload.Body.Close()

				readGate := func() domain.DownloadGate {
					t.Helper()
					response := f.request(t, http.MethodGet, scope.path, nil, true)
					defer response.Body.Close()
					assertStatus(t, response, http.StatusOK)
					var gate domain.DownloadGate
					if err := json.NewDecoder(response.Body).Decode(&gate); err != nil {
						t.Fatal(err)
					}
					return gate
				}
				before := readGate()
				if before.Managed != managed || len(before.Criteria) != 1 {
					t.Fatalf("gate setup: %+v", before)
				}
				for _, body := range []string{`{}`, `{"criteria":null}`, `{"enabled":false}`, `{"criteria":{}}`} {
					response := f.request(t, http.MethodPut, scope.path+"?force=true", []byte(body), true)
					assertStatus(t, response, http.StatusBadRequest)
					var problem httpx.ProblemDetails
					err := json.NewDecoder(response.Body).Decode(&problem)
					response.Body.Close()
					if err != nil || problem.Code != "invalid_json" {
						t.Fatalf("invalid request %s: problem=%+v err=%v", body, problem, err)
					}
					if after := readGate(); !reflect.DeepEqual(before, after) {
						t.Fatalf("invalid request %s changed gate or ownership: before=%+v after=%+v", body, before, after)
					}
					denied := f.request(t, http.MethodGet, "/repository/raw/unscanned", nil, true)
					assertStatus(t, denied, http.StatusForbidden)
					denied.Body.Close()
				}

				cleared := f.request(t, http.MethodPut, scope.path+"?force=true", []byte(`{"criteria":[]}`), true)
				assertStatus(t, cleared, http.StatusOK)
				cleared.Body.Close()
				if gate := readGate(); len(gate.Criteria) != 0 || gate.Managed {
					t.Fatalf("explicit empty array did not clear gate: %+v", gate)
				}
				released := f.request(t, http.MethodGet, "/repository/raw/unscanned", nil, true)
				assertStatus(t, released, http.StatusOK)
				released.Body.Close()
			})
		}
	}
}
