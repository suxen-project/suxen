package content

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestBearerChallengeProxyExchange(t *testing.T) {
	tests := []struct {
		name       string
		headers    []string
		realmHost  string
		service    string
		scope      string
		wantStatus int
	}{
		{"separate fields", []string{`Basic realm="registry"`, `Bearer realm="https://registry.example/token",scope="repository:app:pull"`}, "registry.example", "", "repository:app:pull", 200},
		{"parameters across fields", []string{`Basic realm="registry"`, `Bearer realm="https://registry.example/token"`, `scope="repository:app:pull,push"`, `service="registry"`}, "registry.example", "registry", "repository:app:pull,push", 200},
		{"Basic parameters stay with Basic", []string{`Basic realm="registry"`, `scope="basic-only"`, `Bearer realm="https://registry.example/token"`}, "registry.example", "", "", 200},
		{"combined challenges", []string{`Basic realm="registry", Bearer realm="https://registry.example/token",scope="repository:app:pull"`}, "registry.example", "", "repository:app:pull", 200},
		{"quoted comma", []string{`Bearer realm="https://registry.example/token",scope="repository:app:pull,push"`}, "registry.example", "", "repository:app:pull,push", 200},
		{"quoted escapes and whitespace", []string{"Digest realm=\"other,realm\",\t bEaReR\t realm = \"https://registry.example/token\" , service = \"reg\\\"istry\" , scope = \"repository:app:pull,push\""}, "registry.example", `reg"istry`, "repository:app:pull,push", 200},
		{"bearer after another bearer", []string{`Bearer realm="", Basic realm="registry", Bearer realm="https://registry.example/token"`}, "registry.example", "", "", 200},
		{"foreign realm", []string{`Basic realm="registry", Bearer realm="https://auth.example/token",service=registry`}, "auth.example", "registry", "", 200},
		{"malformed then valid field", []string{`Bearer realm="unfinished`, `Bearer realm="https://registry.example/token"`}, "registry.example", "", "", 200},
		{"valid before malformed field", []string{`Bearer realm="https://registry.example/token"`, `Basic realm="unfinished`}, "registry.example", "", "", 200},
		{"malformed continuation field", []string{`Bearer realm="https://registry.example/token"`, `scope="unfinished`}, "", "", "", 401},
		{"duplicate realm", []string{`Bearer realm="https://registry.example/token",realm="https://auth.example/token"`}, "", "", "", 401},
		{"malformed parameter", []string{`Bearer realm="https://registry.example/token",scope="pull" junk`}, "", "", "", 401},
		{"basic only", []string{`Basic realm="registry"`}, "", "", "", 401},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokenCalls := 0
			retryCalls := 0
			rt := &Runtime{http: &http.Client{Transport: proxyTargetRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := "ok"
				header := make(http.Header)
				switch {
				case r.URL.Path == "/token":
					tokenCalls++
					if r.URL.Hostname() != tc.realmHost || r.URL.Query().Get("scope") != tc.scope || r.URL.Query().Get("service") != tc.service {
						t.Errorf("token URL = %s, want host %q service %q scope %q", r.URL, tc.realmHost, tc.service, tc.scope)
					}
					// The upstream URL contains Basic credentials. They may reach only a same-origin realm.
					_, _, basic := r.BasicAuth()
					if basic != (tc.realmHost == "registry.example") {
						t.Errorf("realm %q received Basic credentials = %v", tc.realmHost, basic)
					}
					body = `{"token":"registry-token"}`
				case r.Header.Get("Authorization") == "Bearer registry-token":
					retryCalls++
				default:
					status = http.StatusUnauthorized
					for _, value := range tc.headers {
						header.Add("WWW-Authenticate", value)
					}
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			request := &http.Request{Header: make(http.Header)}
			response, err := rt.DoUpstreamRequest(request, domain.Repository{Format: "oci", Type: "proxy", Upstream: "https://reader:secret@registry.example"}, http.MethodGet, "v2/app/manifests/latest", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.wantStatus || tokenCalls != retryCalls || tokenCalls != boolCount(tc.wantStatus == 200) {
				t.Errorf("status=%d tokenCalls=%d retryCalls=%d; want status=%d", response.StatusCode, tokenCalls, retryCalls, tc.wantStatus)
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
