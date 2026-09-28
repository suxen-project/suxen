package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// The policy value and asset attribute take different JSON/SQLite routes. Their
// nested number spellings must still have the same value for every predicate
// consumer, while a different number must remain distinct.
func TestCompoundNumericPredicatesAcrossServerRoutes(t *testing.T) {
	fixture := newServerFixture(t)
	for _, name := range []string{"matching.bin", "different.bin"} {
		response := fixture.request(t, http.MethodPut, "/repository/raw/"+name, []byte(name), true)
		assertStatus(t, response, http.StatusCreated)
		response.Body.Close()
	}
	for name, value := range map[string]string{"matching.bin": `{"items":[{"count":1.0}]}`, "different.bin": `{"items":[{"count":2}]}`} {
		asset, err := fixture.Metadata.Asset(context.Background(), "raw", name)
		if err != nil {
			t.Fatal(err)
		}
		response := fixture.requestWithBearer(t, http.MethodPut,
			fmt.Sprintf("/api/v1/repositories/raw/assets/%d/attributes/scan", asset.ID),
			[]byte(`{"result":`+value+`}`), "application/json", testToken)
		assertStatus(t, response, http.StatusCreated)
		response.Body.Close()
	}

	const expected = `{"items":[{"count":1}]}`
	for _, test := range []struct {
		op       string
		value    string
		matching int
		other    int
	}{
		{"=", expected, http.StatusOK, http.StatusForbidden},
		{"!=", expected, http.StatusForbidden, http.StatusOK},
		{"in", `[` + expected + `]`, http.StatusOK, http.StatusForbidden},
		{"not-in", `[` + expected + `]`, http.StatusForbidden, http.StatusOK},
	} {
		t.Run(test.op, func(t *testing.T) {
			body := fmt.Sprintf(`{"enabled":true,"criteria":[{"path":"scan.result","op":%q,"value":%s}]}`, test.op, test.value)
			gate := fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/raw/download-gate", []byte(body), "application/json", testToken)
			if gate.StatusCode != http.StatusCreated && gate.StatusCode != http.StatusOK {
				t.Fatalf("save gate: status %d", gate.StatusCode)
			}
			gate.Body.Close()
			for name, want := range map[string]int{"matching.bin": test.matching, "different.bin": test.other} {
				response := fixture.request(t, http.MethodGet, "/repository/raw/"+name, nil, true)
				assertStatus(t, response, want)
				response.Body.Close()
			}
		})
	}

	classification := fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/raw/classification",
		[]byte(`{"rules":[{"when":[{"path":"scan.result","op":"=","value":`+expected+`}],"key":"numeric","value":"matched"}]}`),
		"application/json", testToken)
	assertStatus(t, classification, http.StatusOK)
	classification.Body.Close()
	for name, want := range map[string]string{"matching.bin": "matched", "different.bin": ""} {
		asset, err := fixture.Metadata.Asset(context.Background(), "raw", name)
		if err != nil {
			t.Fatal(err)
		}
		if got := classificationLabelForKey(asset.Attributes, "numeric"); got != want {
			t.Errorf("%s classification = %q, want %q", name, got, want)
		}
	}

	search := fixture.request(t, http.MethodGet,
		"/api/v1/search?repository=raw&attribute="+url.QueryEscape("scan.result="+expected), nil, true)
	assertStatus(t, search, http.StatusOK)
	var results httpx.CollectionPage[repositoryBrowseItem]
	if err := json.NewDecoder(search.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	search.Body.Close()
	if len(results.Items) != 1 || results.Items[0].Asset.Path != "matching.bin" {
		t.Fatalf("compound numeric search = %+v, want matching.bin", results.Items)
	}

	policy := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/cleanup-policies",
		[]byte(`{"name":"numeric-match","repositories":["raw"],"criteria":[{"path":"scan.result","op":"=","value":`+expected+`}],"action":"delete","enabled":false}`),
		"application/json", testToken)
	assertStatus(t, policy, http.StatusCreated)
	policy.Body.Close()
	preview := fixture.requestWithBearer(t, http.MethodPost,
		"/api/v1/repositories/raw/cleanup?policy=numeric-match&dryRun=true", nil, "", testToken)
	assertStatus(t, preview, http.StatusOK)
	var task domain.Task
	if err := json.NewDecoder(preview.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	preview.Body.Close()
	if task.Result["matched"] != float64(1) {
		t.Fatalf("cleanup preview = %+v, want one match", task)
	}
}

func classificationLabelForKey(attributes map[string]any, key string) string {
	classification, _ := attributes["classification"].(map[string]any)
	value, _ := classification[key].(string)
	return value
}
