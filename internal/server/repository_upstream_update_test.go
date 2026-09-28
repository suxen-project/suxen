package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/httpx"
)

func TestProxyUpdatePreservesOmittedUpstreamAndRotatesExplicitCredentials(t *testing.T) {
	fixture := newServerFixture(t)
	const path = "/api/v1/repositories/signed-proxy"
	const original = "https://first:old-secret@upstream.example/artifacts?token=query-secret"
	const rotated = "https://second:new-secret@upstream.example/artifacts?token=query-secret"

	create := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/repositories", []byte(`{"name":"signed-proxy","format":"oci","type":"proxy","upstream":"`+original+`"}`), "application/json", true)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", create.StatusCode)
	}
	create.Body.Close()
	assertStoredUpstream := func(want string) {
		t.Helper()
		stored, err := fixture.Metadata.Repository(context.Background(), "signed-proxy")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Upstream != want {
			t.Fatalf("stored upstream = %q, want %q", stored.Upstream, want)
		}
	}
	assertStoredUpstream(original)

	// A normal resource read redacts both userinfo and query credentials. An
	// update may omit upstream after reading that representation.
	read := fixture.request(t, http.MethodGet, path, nil, true)
	var redacted map[string]any
	if err := json.NewDecoder(read.Body).Decode(&redacted); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if read.StatusCode != http.StatusOK || redacted["upstream"] != "https://upstream.example/artifacts" {
		t.Fatalf("read status = %d, upstream = %v", read.StatusCode, redacted["upstream"])
	}

	update := fixture.requestWithContentType(t, http.MethodPut, path, []byte(`{"format":"oci","type":"proxy","endpoints":{"hosts":["registry.example"]}}`), "application/json", true)
	if update.StatusCode != http.StatusOK {
		t.Fatalf("omitted-upstream update status = %d, want 200", update.StatusCode)
	}
	update.Body.Close()
	assertStoredUpstream(original)
	stored, err := fixture.Metadata.Repository(context.Background(), "signed-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Endpoints == nil || len(stored.Endpoints.Hosts) != 1 || stored.Endpoints.Hosts[0] != "registry.example" {
		t.Fatalf("unrelated endpoint edit was not saved: %+v", stored.Endpoints)
	}

	rotate := fixture.requestWithContentType(t, http.MethodPut, path, []byte(`{"format":"oci","type":"proxy","upstream":"`+rotated+`","endpoints":{"hosts":["registry.example"]}}`), "application/json", true)
	if rotate.StatusCode != http.StatusOK {
		t.Fatalf("explicit credential rotation status = %d, want 200", rotate.StatusCode)
	}
	rotate.Body.Close()
	assertStoredUpstream(rotated)
	withoutUserinfo := "https://upstream.example/artifacts?token=query-secret"
	removeUserinfo := fixture.requestWithContentType(t, http.MethodPut, path, []byte(`{"format":"oci","type":"proxy","upstream":"`+withoutUserinfo+`","endpoints":{"hosts":["registry.example"]}}`), "application/json", true)
	if removeUserinfo.StatusCode != http.StatusOK {
		t.Fatalf("explicit credential removal status = %d, want 200", removeUserinfo.StatusCode)
	}
	removeUserinfo.Body.Close()
	assertStoredUpstream(withoutUserinfo)

	// A changed query is an endpoint change, even if the hostname and path match.
	changedEndpoint := fixture.requestWithContentType(t, http.MethodPut, path, []byte(`{"format":"oci","type":"proxy","upstream":"https://second:new-secret@upstream.example/artifacts?token=different"}`), "application/json", true)
	defer changedEndpoint.Body.Close()
	if changedEndpoint.StatusCode != http.StatusConflict {
		t.Fatalf("changed endpoint status = %d, want 409", changedEndpoint.StatusCode)
	}
	assertStoredUpstream(withoutUserinfo)
}

func TestProxyCreationRequiresExplicitUpstream(t *testing.T) {
	fixture := newServerFixture(t)
	for _, test := range []struct {
		name string
		body string
	}{
		{"post", `{"name":"missing-post-upstream","format":"raw","type":"proxy"}`},
		{"put creation", `{"format":"raw","type":"proxy"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := http.MethodPost
			path := "/api/v1/repositories"
			if strings.HasPrefix(test.name, "put") {
				method = http.MethodPut
				path += "/missing-put-upstream"
			}
			response := fixture.requestWithContentType(t, method, path, []byte(test.body), "application/json", true)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			var problem httpx.ProblemDetails
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != "upstream_required" {
				t.Fatalf("problem code = %q, want upstream_required", problem.Code)
			}
		})
	}
}
