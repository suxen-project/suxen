package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
)

const discoveryResponse = `{"issuer":"https://idp.example","authorization_endpoint":"https://idp.example/auth","token_endpoint":"https://idp.example/token","jwks_uri":"https://idp.example/keys"}`

var discoveryProvider = domain.OIDCProvider{
	Name: "idp", Issuer: "https://idp.example", ClientID: "client",
}

type discoveryTransportFunc func(*http.Request) (*http.Response, error)

func (transport discoveryTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func discoveryHTTPResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(discoveryResponse)),
	}
}

type discoveryResult struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	err      error
}

func startDiscovery(service *Service, ctx context.Context) <-chan discoveryResult {
	return startDiscoveryForProvider(service, ctx, discoveryProvider)
}

func startDiscoveryForProvider(service *Service, ctx context.Context, provider domain.OIDCProvider) <-chan discoveryResult {
	result := make(chan discoveryResult, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/", nil).WithContext(ctx)
		discovered, verifier, err := service.oidcRuntime(r, provider)
		result <- discoveryResult{provider: discovered, verifier: verifier, err: err}
	}()
	return result
}

func TestOIDCDiscoveryBoundsDistinctProvidersGlobally(t *testing.T) {
	entered := make(chan struct{}, maximumConcurrentOIDCDiscoveries)
	release := make(chan struct{})
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{OutboundTimeout: 5 * time.Second},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
				return discoveryHTTPResponse(http.StatusOK), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})},
	})
	results := make([]<-chan discoveryResult, 0, maximumConcurrentOIDCDiscoveries)
	for index := range maximumConcurrentOIDCDiscoveries {
		provider := discoveryProvider
		provider.Name = string(rune('a' + index))
		results = append(results, startDiscoveryForProvider(service, context.Background(), provider))
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("outbound discovery did not start")
		}
	}
	fifth := discoveryProvider
	fifth.Name = "fifth"
	if got := awaitDiscovery(t, startDiscoveryForProvider(service, context.Background(), fifth)); !errors.Is(got.err, errOIDCDiscoveryBusy) {
		t.Fatalf("fifth provider error = %v, want capacity error", got.err)
	}
	if got := calls.Load(); got != maximumConcurrentOIDCDiscoveries {
		t.Fatalf("outbound discovery calls = %d, want %d", got, maximumConcurrentOIDCDiscoveries)
	}
	close(release)
	for _, result := range results {
		if got := awaitDiscovery(t, result); got.err != nil {
			t.Fatalf("admitted discovery failed: %v", got.err)
		}
	}
	if got := awaitDiscovery(t, startDiscoveryForProvider(service, context.Background(), fifth)); got.err != nil {
		t.Fatalf("capacity did not recover after completion: %v", got.err)
	}
	if got := calls.Load(); got != maximumConcurrentOIDCDiscoveries+1 {
		t.Fatalf("outbound calls after release = %d, want %d", got, maximumConcurrentOIDCDiscoveries+1)
	}
}

func TestOIDCDiscoveryTimeoutReleasesCapacityForRetry(t *testing.T) {
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{OutboundTimeout: 40 * time.Millisecond},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return discoveryHTTPResponse(http.StatusOK), nil
		})},
	})
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err == nil {
		t.Fatal("timed-out discovery unexpectedly succeeded")
	}
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err != nil {
		t.Fatalf("discovery could not retry after timeout: %v", got.err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("outbound calls after timeout and retry = %d, want 2", got)
	}
}

func awaitDiscovery(t *testing.T, result <-chan discoveryResult) discoveryResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("OIDC discovery did not finish")
		return discoveryResult{}
	}
}

func awaitDiscoveryWaiters(t *testing.T, service *Service, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		service.oidcMu.Lock()
		got := 0
		for key, flight := range service.oidcDiscoveries {
			if key.name == discoveryProvider.Name {
				got += flight.waiters
			}
		}
		service.oidcMu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("OIDC discovery waiters never reached %d", want)
}

func TestOIDCDiscoveryCoalescesAndBoundsWaiters(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{OutboundTimeout: 5 * time.Second},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
				return discoveryHTTPResponse(http.StatusOK), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})},
	})
	results := []<-chan discoveryResult{startDiscovery(service, context.Background())}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("outbound discovery did not start")
	}
	for range maximumOIDCDiscoveryWaitersPerProvider - 1 {
		results = append(results, startDiscovery(service, context.Background()))
	}
	awaitDiscoveryWaiters(t, service, maximumOIDCDiscoveryWaitersPerProvider)
	busy := awaitDiscovery(t, startDiscovery(service, context.Background()))
	if !errors.Is(busy.err, errOIDCDiscoveryBusy) {
		t.Fatalf("excess waiter error = %v, want capacity error", busy.err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("outbound discoveries while blocked = %d, want 1", got)
	}
	close(release)
	for _, result := range results {
		got := awaitDiscovery(t, result)
		if got.err != nil || got.provider == nil || got.verifier == nil {
			t.Fatalf("shared discovery result = %+v", got)
		}
	}
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err != nil {
		t.Fatalf("cached discovery failed: %v", got.err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached verifier triggered %d outbound calls, want 1", got)
	}
}

type discoveryAuthenticationStore struct{ Metadata }

func (discoveryAuthenticationStore) AuthenticateToken(context.Context, string) (domain.User, bool) {
	return domain.User{}, false
}

func (discoveryAuthenticationStore) OIDCProviderByIssuer(context.Context, string) (domain.OIDCProvider, error) {
	return discoveryProvider, nil
}

func TestBearerBurstSharesColdOIDCDiscovery(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{
			AuthFailureLimit: 2, AuthFailureWindow: time.Minute,
			AuthLockout: time.Minute, OutboundTimeout: 5 * time.Second,
		},
		Metadata: discoveryAuthenticationStore{},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
				return discoveryHTTPResponse(http.StatusOK), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})},
	})
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://idp.example"}`)) + ".e30"
	start := func() <-chan bool {
		result := make(chan bool, 1)
		go func() {
			r := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/", nil)
			r.RemoteAddr = "192.0.2.10:12345"
			r.Header.Set("Authorization", "Bearer "+token)
			_, authenticated := service.Authenticate(r)
			result <- authenticated
		}()
		return result
	}
	results := []<-chan bool{start()}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("outbound discovery did not start")
	}
	for range maximumOIDCDiscoveryWaitersPerProvider - 1 {
		results = append(results, start())
	}
	awaitDiscoveryWaiters(t, service, maximumOIDCDiscoveryWaitersPerProvider)
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent bearer attempts started %d discoveries, want 1", got)
	}
	close(release)
	for _, result := range results {
		select {
		case authenticated := <-result:
			if authenticated {
				t.Fatal("invalid bearer token authenticated")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("bearer authentication did not finish")
		}
	}
}

func TestOIDCDiscoverySurvivesFirstCallerCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	service := New(Options{
		Config: config.Config{OutboundTimeout: 5 * time.Second},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return discoveryHTTPResponse(http.StatusOK), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})},
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := startDiscovery(service, ctx)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("outbound discovery did not start")
	}
	second := startDiscovery(service, context.Background())
	awaitDiscoveryWaiters(t, service, 2)
	cancel()
	if got := awaitDiscovery(t, first); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled caller error = %v, want context.Canceled", got.err)
	}
	close(release)
	if got := awaitDiscovery(t, second); got.err != nil || got.verifier == nil {
		t.Fatalf("remaining caller lost discovery: %+v", got)
	}
}

func TestOIDCDiscoveryFailureCanRetry(t *testing.T) {
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{OutboundTimeout: time.Second},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return discoveryHTTPResponse(http.StatusServiceUnavailable), nil
			}
			return discoveryHTTPResponse(http.StatusOK), nil
		})},
	})
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err == nil {
		t.Fatal("failed discovery unexpectedly succeeded")
	}
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err != nil {
		t.Fatalf("discovery did not retry after failure: %v", got.err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("outbound discovery attempts = %d, want 2", got)
	}
}

func TestOIDCDiscoveryInvalidationStartsFreshAttempt(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	service := New(Options{
		Config: config.Config{OutboundTimeout: 5 * time.Second},
		HTTPClient: &http.Client{Transport: discoveryTransportFunc(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return discoveryHTTPResponse(http.StatusOK), nil
		})},
	})
	stale := startDiscovery(service, context.Background())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial discovery did not start")
	}
	service.InvalidateOIDCVerifier(discoveryProvider.Name)
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err != nil {
		t.Fatalf("fresh discovery after invalidation failed: %v", got.err)
	}
	close(release)
	if got := awaitDiscovery(t, stale); got.err != nil {
		t.Fatalf("original discovery failed: %v", got.err)
	}
	if got := awaitDiscovery(t, startDiscovery(service, context.Background())); got.err != nil {
		t.Fatalf("cached discovery failed: %v", got.err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("outbound attempts = %d, want two generations", got)
	}
}
