package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// These literals pass through JSON requests and SQLite before evaluation. The
// equivalent spellings exceed the exponent range of big.Rat.SetString.
func TestOverrangeNumbersAcrossHTTPPolicyRoutes(t *testing.T) {
	fixture := newServerFixture(t)
	for _, name := range []string{"equal.bin", "greater.bin"} {
		response := fixture.request(t, http.MethodPut, "/repository/raw/"+name, []byte(name), true)
		assertStatus(t, response, http.StatusCreated)
		response.Body.Close()
	}
	for name, numbers := range map[string]struct{ value, tower string }{
		"equal.bin":   {"10e1000001", "10e9999999999999999999999998"},
		"greater.bin": {"1e1000003", "1e10000000000000000000000000"},
	} {
		asset, err := fixture.Metadata.Asset(context.Background(), "raw", name)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"value":%s,"tower":%s,"nested":{"items":[{"count":%s}]},"values":[%s]}`,
			numbers.value, numbers.tower, numbers.value, numbers.value)
		response := fixture.requestWithBearer(t, http.MethodPut,
			fmt.Sprintf("/api/v1/repositories/raw/assets/%d/attributes/scan", asset.ID),
			[]byte(body), "application/json", testToken)
		if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
			t.Fatalf("write %s annotation: status %d", name, response.StatusCode)
		}
		response.Body.Close()
	}

	const equal = "1e1000002"
	const tower = "1e9999999999999999999999999"
	for _, test := range []struct {
		name, path, op, value string
		equalOK, greaterOK    bool
	}{
		{"equal", "scan.value", "=", equal, true, false},
		{"not equal", "scan.value", "!=", equal, false, true},
		{"in", "scan.value", "in", `[0,` + equal + `]`, true, false},
		{"not in bypass", "scan.value", "not-in", `[` + equal + `]`, false, true},
		{"less than", "scan.value", "<", equal, false, false},
		{"less than or equal", "scan.value", "<=", equal, true, false},
		{"greater than", "scan.value", ">", equal, false, true},
		{"greater than or equal", "scan.value", ">=", equal, true, true},
		{"contains", "scan.values", "contains", equal, true, false},
		{"nested object and array", "scan.nested", "=", `{"items":[{"count":` + equal + `}]}`, true, false},
		{"exponent beyond int64", "scan.tower", "=", tower, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"enabled":true,"criteria":[{"path":%q,"op":%q,"value":%s}]}`, test.path, test.op, test.value)
			response := fixture.requestWithBearer(t, http.MethodPut,
				"/api/v1/repositories/raw/download-gate", []byte(body), "application/json", testToken)
			if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
				t.Fatalf("save gate: status %d", response.StatusCode)
			}
			response.Body.Close()
			for name, allowed := range map[string]bool{"equal.bin": test.equalOK, "greater.bin": test.greaterOK} {
				want := http.StatusForbidden
				if allowed {
					want = http.StatusOK
				}
				read := fixture.request(t, http.MethodGet, "/repository/raw/"+name, nil, true)
				if read.StatusCode != want {
					t.Errorf("%s: status %d, want %d", name, read.StatusCode, want)
				}
				read.Body.Close()
			}
		})
	}

	classification := fixture.requestWithBearer(t, http.MethodPut, "/api/v1/repositories/raw/classification",
		[]byte(`{"rules":[{"when":[{"path":"scan.value","op":"=","value":`+equal+`}],"key":"range","value":"equal"}]}`),
		"application/json", testToken)
	assertStatus(t, classification, http.StatusOK)
	classification.Body.Close()
	for name, want := range map[string]string{"equal.bin": "equal", "greater.bin": ""} {
		asset, err := fixture.Metadata.Asset(context.Background(), "raw", name)
		if err != nil {
			t.Fatal(err)
		}
		if got := classificationLabelForKey(asset.Attributes, "range"); got != want {
			t.Errorf("%s classification = %q, want %q", name, got, want)
		}
	}

	search := fixture.request(t, http.MethodGet,
		"/api/v1/search?repository=raw&attribute="+url.QueryEscape("scan.value="+equal), nil, true)
	assertStatus(t, search, http.StatusOK)
	var page httpx.CollectionPage[repositoryBrowseItem]
	decoder := json.NewDecoder(search.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&page); err != nil {
		t.Fatal(err)
	}
	search.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Asset.Path != "equal.bin" {
		t.Fatalf("overrange search returned %+v, want equal.bin", page.Items)
	}
	search = fixture.request(t, http.MethodGet,
		"/api/v1/search?repository=raw&attribute="+url.QueryEscape("scan.tower="+tower), nil, true)
	assertStatus(t, search, http.StatusOK)
	page = httpx.CollectionPage[repositoryBrowseItem]{}
	decoder = json.NewDecoder(search.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&page); err != nil {
		t.Fatal(err)
	}
	search.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Asset.Path != "equal.bin" {
		t.Fatalf("beyond-int64 exponent search returned %+v, want equal.bin", page.Items)
	}

	policy := fixture.requestWithBearer(t, http.MethodPost, "/api/v1/cleanup-policies",
		[]byte(`{"name":"overrange-match","repositories":["raw"],"criteria":[{"path":"scan.value","op":"=","value":`+equal+`}],"action":"delete","enabled":false}`),
		"application/json", testToken)
	assertStatus(t, policy, http.StatusCreated)
	policy.Body.Close()
	preview := fixture.requestWithBearer(t, http.MethodPost,
		"/api/v1/repositories/raw/cleanup?policy=overrange-match&dryRun=true", nil, "", testToken)
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

func TestAnonymousSearchWithMillionExponent(t *testing.T) {
	fixture := newServerFixtureWithAnonymousRead(t)
	for index := range 10 {
		response := fixture.request(t, http.MethodPut,
			fmt.Sprintf("/repository/raw/cost-%d", index), []byte("x"), true)
		assertStatus(t, response, http.StatusCreated)
		response.Body.Close()
	}
	for _, literal := range []string{"1e1000000", "1e-1000000"} {
		start := time.Now()
		response := fixture.request(t, http.MethodGet,
			"/api/v1/search?repository=raw&attribute="+url.QueryEscape("sys.size="+literal), nil, false)
		assertStatus(t, response, http.StatusOK)
		var page httpx.CollectionPage[repositoryBrowseItem]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(page.Items) != 0 {
			t.Fatalf("anonymous search for %s returned %+v, want no matches", literal, page.Items)
		}
		t.Logf("anonymous search over 10 assets, %s: %s", literal, time.Since(start))
	}
}
