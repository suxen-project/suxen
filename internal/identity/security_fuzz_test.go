package identity

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type fuzzAuthenticationStore struct{ store.Store }

func (fuzzAuthenticationStore) AuthenticatePassword(context.Context, string, string) (domain.User, bool) {
	return domain.User{}, false
}

func (fuzzAuthenticationStore) AuthenticateToken(context.Context, string) (domain.User, bool) {
	return domain.User{}, false
}

func (fuzzAuthenticationStore) OIDCProviders(context.Context) ([]domain.OIDCProvider, error) {
	return nil, nil
}

func (fuzzAuthenticationStore) OIDCProviderByIssuer(context.Context, string) (domain.OIDCProvider, error) {
	return domain.OIDCProvider{}, domain.ErrNotFound
}

func FuzzAuthorizationHeaderParsing(f *testing.F) {
	for _, seed := range []string{"Bearer token", "bare-cargo-token", "Basic YWRtaW46cGFzcw==", "", "Bearer"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		service := New(Options{
			Config: config.Config{
				AuthFailureLimit: 100, AuthFailureWindow: time.Minute, AuthLockout: time.Minute,
			},
			Metadata: fuzzAuthenticationStore{},
		})
		request := httptest.NewRequest("GET", "http://registry.example/v2/", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		request.Header.Set("Authorization", header)
		_, _, kind := service.AuthenticateWithKind(request)
		if kind == "" {
			t.Fatal("authentication parser returned an empty credential kind")
		}
	})
}
