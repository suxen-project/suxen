package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestServerSkipsCredentialVerificationDuringLockout(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	metadata := &countingAuthenticationStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(metadata)
	fixture.Handler.identity.ReplaceFailureLimiter(2, time.Minute, 5*time.Minute)

	for attempt := 1; attempt <= 3; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
		request.SetBasicAuth("admin", "incorrect-password")
		response := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt, response.Code)
		}
	}
	if attempts := metadata.passwordAttempts.Load(); attempts != 2 {
		t.Fatalf("password verifications = %d, want 2 before lockout", attempts)
	}
}

func TestServerThrottlesInvalidBareTokens(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	metadata := &countingAuthenticationStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(metadata)
	fixture.Handler.identity.ReplaceFailureLimiter(2, time.Minute, 5*time.Minute)

	for attempt := 0; attempt < 3; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
		request.Header.Set("Authorization", "invalid-cargo-token")
		response := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt+1, response.Code)
		}
	}
	if attempts := metadata.tokenAttempts.Load(); attempts != 2 {
		t.Fatalf("token verifications = %d, want 2 before lockout", attempts)
	}
}

func TestAnonymousOCITokenCannotResetAdminPasswordFailureBudget(t *testing.T) {
	t.Parallel()
	fixture := newServerFixtureWithAnonymousRead(t)
	metadata := &countingAuthenticationStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(metadata)
	fixture.Handler.identity.ReplaceFailureLimiter(3, time.Minute, 5*time.Minute)

	tokenResponse := fixture.request(
		t, http.MethodGet, "/v2/token?scope=repository:acme/app:pull", nil, false,
	)
	if tokenResponse.StatusCode != http.StatusOK {
		t.Fatalf("anonymous pull token status = %d, want 200", tokenResponse.StatusCode)
	}
	token := decodeOCIAccessToken(t, tokenResponse)

	requestAsPeer := func(password, bearer string) int {
		path := "/api/v1/stats"
		if bearer != "" {
			path = "/api/v1/whoami"
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "198.51.100.7:12345"
		if password != "" {
			request.SetBasicAuth("admin", password)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response := httptest.NewRecorder()
		fixture.Handler.ServeHTTP(response, request)
		return response.Code
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if status := requestAsPeer("incorrect-password", ""); status != http.StatusUnauthorized {
			t.Fatalf("wrong password attempt %d status = %d, want 401", attempt, status)
		}
	}
	if status := requestAsPeer("", token); status != http.StatusOK {
		t.Fatalf("anonymous pull token status = %d, want 200", status)
	}
	if status := requestAsPeer("incorrect-password", ""); status != http.StatusUnauthorized {
		t.Fatalf("third wrong password status = %d, want 401", status)
	}
	if status := requestAsPeer("test-password", ""); status != http.StatusUnauthorized {
		t.Fatalf("correct password during lockout status = %d, want 401", status)
	}
	if attempts := metadata.passwordAttempts.Load(); attempts != 3 {
		t.Fatalf("password verifications = %d, want 3 before lockout", attempts)
	}
}

func TestServerBoundsConcurrentPasswordChecks(t *testing.T) {
	fixture := newServerFixture(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	metadata := &blockingPasswordStore{
		Store: fixture.Metadata, entered: make(chan struct{}, 16), release: release,
	}
	fixture.Handler.setMetadata(metadata)
	fixture.Handler.identity.ReplaceFailureLimiter(10, time.Minute, 5*time.Minute)

	request := func(peer string) <-chan int {
		result := make(chan int, 1)
		go func() {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			r.RemoteAddr = peer + ":12345"
			r.SetBasicAuth("admin", "incorrect-password")
			response := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(response, r)
			result <- response.Code
		}()
		return result
	}
	waitEntered := func() {
		t.Helper()
		select {
		case <-metadata.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("password verification did not start")
		}
	}
	waitStatus := func(result <-chan int) {
		t.Helper()
		select {
		case status := <-result:
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("request did not complete")
		}
	}

	first := request("192.0.2.10")
	second := request("192.0.2.10")
	waitEntered()
	waitEntered()
	third := request("192.0.2.11")
	fourth := request("192.0.2.12")
	waitEntered()
	waitEntered()
	queuedPeer := request("192.0.2.10")
	queuedGlobal := request("192.0.2.13")
	select {
	case status := <-queuedPeer:
		t.Fatalf("same-peer request completed before capacity was released: %d", status)
	case status := <-queuedGlobal:
		t.Fatalf("global request completed before capacity was released: %d", status)
	case <-time.After(50 * time.Millisecond):
	}
	if attempts := metadata.attempts.Load(); attempts != 4 {
		t.Fatalf("global password checks = %d, want 4", attempts)
	}

	releaseOnce.Do(func() { close(release) })
	for _, result := range []<-chan int{first, second, third, fourth, queuedPeer, queuedGlobal} {
		waitStatus(result)
	}
	if attempts := metadata.attempts.Load(); attempts != 6 {
		t.Fatalf("password checks after release = %d, want 6", attempts)
	}
}

func TestServerAuthenticatesConcurrentBasicRequestsFromOnePeer(t *testing.T) {
	fixture := newServerFixture(t)
	const requests = 8
	start := make(chan struct{})
	results := make(chan int, requests)
	for range requests {
		go func() {
			<-start
			r := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			r.RemoteAddr = "192.0.2.10:12345"
			r.SetBasicAuth("admin", "test-password")
			response := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(response, r)
			results <- response.Code
		}()
	}
	close(start)
	for range requests {
		select {
		case status := <-results:
			if status != http.StatusOK {
				t.Fatalf("valid concurrent Basic request status = %d, want 200", status)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("valid concurrent Basic requests did not complete")
		}
	}
}

type blockingPasswordStore struct {
	store.Store
	entered  chan struct{}
	release  <-chan struct{}
	attempts atomic.Int64
}

func (metadata *blockingPasswordStore) AuthenticatePassword(
	context.Context, string, string,
) (domain.User, bool) {
	metadata.attempts.Add(1)
	metadata.entered <- struct{}{}
	<-metadata.release
	return domain.User{}, false
}

type countingAuthenticationStore struct {
	store.Store
	passwordAttempts atomic.Int64
	tokenAttempts    atomic.Int64
}

func (metadata *countingAuthenticationStore) AuthenticateToken(
	ctx context.Context,
	token string,
) (domain.User, bool) {
	metadata.tokenAttempts.Add(1)
	return metadata.Store.AuthenticateToken(ctx, token)
}

func (metadata *countingAuthenticationStore) AuthenticatePassword(
	ctx context.Context,
	username string,
	password string,
) (domain.User, bool) {
	metadata.passwordAttempts.Add(1)
	return metadata.Store.AuthenticatePassword(ctx, username, password)
}
