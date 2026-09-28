package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStreamingClientAllowsSlowResponseBodyButBoundsHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/headers" {
			time.Sleep(90 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		time.Sleep(90 * time.Millisecond)
		_, _ = w.Write([]byte("second"))
	}))
	defer upstream.Close()
	policy := NewPolicy(Options{AllowedHosts: []string{"127.0.0.1"}})
	streaming := policy.StreamingClient(40 * time.Millisecond)
	if streaming.Timeout != 0 {
		t.Fatalf("streaming client overall timeout = %s, want zero", streaming.Timeout)
	}
	response, err := streaming.Get(upstream.URL + "/body")
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(content) != "firstsecond" {
		t.Fatalf("slow response body = %q, %v", content, err)
	}
	if _, err := streaming.Get(upstream.URL + "/headers"); err == nil {
		t.Fatal("slow response headers exceeded the configured wait")
	}
	bounded := policy.Client(40 * time.Millisecond)
	response, err = bounded.Get(upstream.URL + "/body")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil {
		t.Fatal("control-plane overall timeout did not stop a slow body")
	}
}

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(
	_ context.Context,
	_ string,
	host string,
) ([]netip.Addr, error) {
	addresses, found := r[host]
	if !found {
		return nil, errors.New("host not found")
	}
	return addresses, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPolicyBlocksSensitiveLiteralAddresses(t *testing.T) {
	policy := NewPolicy(Options{})
	for _, rawURL := range []string{
		"http://0.0.0.0/",
		"http://127.0.0.1/",
		"http://10.20.30.40/",
		"http://100.100.100.200/latest/meta-data/",
		"http://169.254.169.254/latest/meta-data/",
		"http://198.18.0.1/",
		"http://240.0.0.1/",
		"http://[::]/",
		"http://[::1]/",
		"http://[64:ff9b::a9fe:a9fe]/",
		"http://[2002:a9fe:a9fe::1]/",
		"http://[fe80::1]/",
		"http://[ff02::1]/",
		"http://[fd00::1]/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			target, err := url.Parse(rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if err := policy.ValidateURL(context.Background(), target); err == nil {
				t.Fatalf("sensitive destination %s was permitted", rawURL)
			}
		})
	}
}

func TestPolicyRejectsDNSAnswerContainingForbiddenAddress(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"mixed.example": {
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("169.254.169.254"),
		},
	}})
	target, err := url.Parse("https://mixed.example/resource")
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.ValidateURL(context.Background(), target); err == nil {
		t.Fatal("mixed public and link-local DNS answer was permitted")
	}
}

func TestPolicyPinsTheValidatedAddressForDial(t *testing.T) {
	resolverCalls := 0
	resolver := resolverFunc(func(
		_ context.Context,
		_ string,
		_ string,
	) ([]netip.Addr, error) {
		resolverCalls++
		return []netip.Addr{netip.MustParseAddr("8.8.4.4")}, nil
	})
	policy := NewPolicy(Options{Resolver: resolver})
	dialedAddress := ""
	policy.dial = func(
		_ context.Context,
		_ string,
		address string,
	) (net.Conn, error) {
		dialedAddress = address
		return nil, errors.New("test dial stopped")
	}

	_, err := policy.dialContext(context.Background(), "tcp", "registry.example:443")
	if err == nil {
		t.Fatal("test dial unexpectedly succeeded")
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver called %d times, want once", resolverCalls)
	}
	if dialedAddress != "8.8.4.4:443" {
		t.Fatalf("dialed %q, want the validated address", dialedAddress)
	}
}

func TestPolicyRevalidatesRedirectDestination(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"public.example": {netip.MustParseAddr("8.8.8.8")},
	}})
	client := policy.Client(time.Second)
	requests := 0
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header: http.Header{
				"Location": {"http://169.254.169.254/latest/meta-data/"},
			},
			Body:    io.NopCloser(strings.NewReader("")),
			Request: request,
		}, nil
	})

	_, err := client.Get("https://public.example/start")
	if err == nil || !strings.Contains(err.Error(), "redirect destination rejected") {
		t.Fatalf("got redirect error %v, want policy rejection", err)
	}
	if requests != 1 {
		t.Fatalf("transport received %d requests, want only the initial request", requests)
	}
}

func TestPolicyStripsSensitiveHeadersAcrossAllowedRedirect(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"first.example":  {netip.MustParseAddr("8.8.8.8")},
		"second.example": {netip.MustParseAddr("8.8.4.4")},
	}})
	client := policy.Client(time.Second)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "first.example" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     http.Header{"Location": {"https://second.example/next"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		}
		for _, header := range []string{
			"Authorization",
			"Cookie",
			"Proxy-Authorization",
			"Referer",
			"X-Suxen-Signature-256",
		} {
			if value := request.Header.Get(header); value != "" {
				t.Errorf("redirect retained %s header %q", header, value)
			}
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})

	request, err := http.NewRequest(http.MethodGet, "https://first.example/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Cookie", "session=secret")
	request.Header.Set("Proxy-Authorization", "Basic secret")
	request.Header.Set("Referer", "https://first.example/private")
	request.Header.Set("X-Suxen-Signature-256", "sha256=secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestPolicyDoesNotRestoreSensitiveHeadersAfterCrossOriginRedirect(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
	}{
		{name: "different port", target: "https://registry.example:8443/first"},
		{name: "different scheme", target: "http://registry.example/first"},
		{name: "different host", target: "https://child.registry.example/first"},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := NewPolicy(Options{Resolver: staticResolver{
				"registry.example":       {netip.MustParseAddr("8.8.8.8")},
				"child.registry.example": {netip.MustParseAddr("8.8.4.4")},
			}})
			client := policy.Client(time.Second)
			requests := 0
			client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				location := ""
				status := http.StatusNoContent
				switch request.URL.Path {
				case "/start":
					location = test.target
					status = http.StatusFound
				case "/first":
					location = "/second"
					status = http.StatusFound
				}
				if requests > 1 {
					for _, header := range []string{
						"Authorization", "Cookie", "Proxy-Authorization",
						"Referer", "X-Suxen-Signature-256",
					} {
						if value := request.Header.Get(header); value != "" {
							t.Errorf("request %d to %s restored %s header %q", requests, request.URL, header, value)
						}
					}
				}
				header := make(http.Header)
				if location != "" {
					header.Set("Location", location)
				}
				return &http.Response{
					StatusCode: status,
					Header:     header,
					Body:       http.NoBody,
					Request:    request,
				}, nil
			})

			request, err := http.NewRequest(http.MethodGet, "https://registry.example/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			for header, value := range map[string]string{
				"Authorization":         "Bearer secret",
				"Cookie":                "session=secret",
				"Proxy-Authorization":   "Basic secret",
				"Referer":               "https://registry.example/private",
				"X-Suxen-Signature-256": "sha256=secret",
			} {
				request.Header.Set(header, value)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if requests != 3 {
				t.Fatalf("requests = %d, want three", requests)
			}
		})
	}
}

func TestPolicyPreservesOriginalCredentialsAfterReturningToOriginalOrigin(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"registry.example": {netip.MustParseAddr("8.8.8.8")},
	}})
	client := policy.Client(time.Second)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		location := ""
		status := http.StatusNoContent
		switch request.URL.Path {
		case "/start":
			location = "https://registry.example:8443/other"
			status = http.StatusFound
		case "/other":
			if got := request.Header.Get("Authorization"); got != "" {
				t.Errorf("cross-origin hop retained Authorization %q", got)
			}
			location = "https://registry.example/final"
			status = http.StatusFound
		case "/final":
			if got := request.Header.Get("Authorization"); got != "Bearer secret" {
				t.Errorf("return to original origin Authorization = %q, want original", got)
			}
		}
		header := make(http.Header)
		if location != "" {
			header.Set("Location", location)
		}
		return &http.Response{StatusCode: status, Header: header, Body: http.NoBody, Request: request}, nil
	})
	request, err := http.NewRequest(http.MethodGet, "https://registry.example/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestPolicyPreservesSensitiveHeadersOnSameOriginRedirect(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"registry.example": {netip.MustParseAddr("8.8.8.8")},
	}})
	client := policy.Client(time.Second)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/start" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     http.Header{"Location": {"/canonical"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("same-origin redirect Authorization = %q, want preserved value", got)
		}
		if got := request.Header.Get("Cookie"); got != "session=secret" {
			t.Errorf("same-origin redirect Cookie = %q, want preserved value", got)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})

	request, err := http.NewRequest(http.MethodGet, "https://registry.example/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Cookie", "session=secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestPolicyRejectsRedirectForRequestWithUnsafeMethod(t *testing.T) {
	policy := NewPolicy(Options{Resolver: staticResolver{
		"second.example": {netip.MustParseAddr("8.8.4.4")},
	}})
	redirect, err := http.NewRequest(http.MethodGet, "https://second.example/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := http.NewRequest(http.MethodPost, "https://first.example/start", strings.NewReader("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.checkRedirect(redirect, []*http.Request{original}); err == nil {
		t.Fatal("redirect for an unsafe original method was permitted")
	}
}

func TestPolicyAllowsExplicitInternalDestinations(t *testing.T) {
	policy := NewPolicy(Options{
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")},
		AllowedHosts: []string{"identity.internal"},
		Resolver: staticResolver{
			"registry.internal": {netip.MustParseAddr("10.20.30.40")},
			"identity.internal": {netip.MustParseAddr("192.168.50.10")},
		},
	})
	for _, rawURL := range []string{
		"https://registry.internal/v2/",
		"https://identity.internal/.well-known/openid-configuration",
	} {
		target, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := policy.ValidateURL(context.Background(), target); err != nil {
			t.Fatalf("explicitly allowed URL %s was rejected: %v", rawURL, err)
		}
	}
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (function resolverFunc) LookupNetIP(
	ctx context.Context,
	network string,
	host string,
) ([]netip.Addr, error) {
	return function(ctx, network, host)
}
