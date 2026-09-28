package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionRedirectCannotExposeSecrets(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, target := range []string{
			"http://suxen.example/api/v1/provision",
			"https://other.example/api/v1/provision",
			"https://child.suxen.example/api/v1/provision",
			"https://suxen.example:8443/api/v1/provision",
		} {
			t.Run(http.StatusText(status)+"/"+target, func(t *testing.T) {
				documentPath := filepath.Join(t.TempDir(), "desired.yaml")
				t.Setenv("SUXEN_TEST_REDIRECT_PASSWORD", "resolved-test-password")
				document := "resources:\n  - kind: user\n    name: automation\n    spec:\n      secretRef:\n        env: SUXEN_TEST_REDIRECT_PASSWORD\n"
				if err := os.WriteFile(documentPath, []byte(document), 0o600); err != nil {
					t.Fatal(err)
				}
				httpClient := newHTTPClient()
				requests := 0
				httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if requests > 1 {
						t.Fatal("redirect transmitted a request outside the configured origin")
					}
					if !strings.Contains(string(body), "resolved-test-password") || r.Header.Get("Authorization") != "Bearer test-token" {
						t.Fatal("initial provisioning request did not contain expected test credentials")
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Location": {target}}, Body: http.NoBody, Request: r}, nil
				})
				api := &client{baseURL: "https://suxen.example", token: "test-token", http: httpClient}
				if err := applyCommand(api, []string{"-f", documentPath}); err == nil || !strings.Contains(err.Error(), "configured server origin") {
					t.Fatalf("apply error = %v; want rejected redirect", err)
				}
				if requests != 1 {
					t.Fatalf("requests = %d; want initial request only", requests)
				}
			})
		}
	}
}

func TestClientAllowsSameOriginRedirects(t *testing.T) {
	for _, test := range []struct{ origin, target string }{
		{"https://suxen.example", "/canonical"},
		{"https://suxen.example", "https://SUXEN.example:443/canonical"},
		{"http://localhost:8080", "http://localhost:8080/canonical"},
		{"http://[::1]", "http://[::1]:80/canonical"},
	} {
		t.Run(test.origin+test.target, func(t *testing.T) {
			httpClient := newHTTPClient()
			requests := 0
			httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {test.target}}, Body: http.NoBody, Request: r}, nil
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "payload" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Fatalf("same-origin redirect lost request: method=%s body=%q error=%v", r.Method, body, err)
				}
				return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
			})
			api := &client{baseURL: test.origin, token: "test-token", http: httpClient}
			response, err := api.do(http.MethodPost, "/initial", strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if requests != 2 {
				t.Fatalf("requests = %d; want one redirect", requests)
			}
		})
	}
}

func TestClientBoundsRedirectLoops(t *testing.T) {
	httpClient := newHTTPClient()
	requests := 0
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"/loop"}}, Body: http.NoBody, Request: r}, nil
	})
	api := &client{baseURL: "https://suxen.example", http: httpClient}
	_, err := api.do(http.MethodGet, "/loop", nil)
	if err == nil || !strings.Contains(err.Error(), "10 redirects") || requests != 10 {
		t.Fatalf("requests=%d error=%v; want bounded redirect loop", requests, err)
	}
}
