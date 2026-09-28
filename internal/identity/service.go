package identity

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// Metadata contains only the account and OIDC reads required for authentication.
type Metadata interface {
	AuthenticatePassword(context.Context, string, string) (domain.User, bool)
	AuthenticateToken(context.Context, string) (domain.User, bool)
	EffectivePrivileges(context.Context, string) ([]string, error)
	LocalAuthorization(context.Context, string, string) (domain.User, []string, error)
	PrivilegesForRoles(context.Context, []string) ([]string, error)
	OIDCProviderByIssuer(context.Context, string) (domain.OIDCProvider, error)
	OIDCProvider(context.Context, string) (domain.OIDCProvider, error)
	OIDCProviders(context.Context) ([]domain.OIDCProvider, error)
	User(context.Context, string) (domain.User, error)
}

// Counter is the Prometheus-shaped increment used for auth metrics.
type Counter interface {
	Inc()
}

// Service authenticates callers, enforces privileges, and runs OIDC login.
// It owns its dependencies; the compositor hands them over once in Options
// and pushes later changes through the Set methods. It does not point back
// at the HTTP compositor.
type Service struct {
	Config   config.Config
	Failures Counter
	Blocked  Counter
	Log      *slog.Logger

	metadata              Metadata
	http                  *http.Client
	authFailures          *authenticationFailureLimiter
	oidcMu                sync.Mutex
	oidcVerifiers         map[string]cachedOIDCVerifier
	oidcDiscoveries       map[oidcDiscoveryKey]*oidcDiscoveryFlight
	oidcGenerations       map[string]uint64
	oidcLatestFingerprint map[string]string
	activeOIDCDiscoveries int
}

// Options are the dependencies of a Service.
type Options struct {
	Config     config.Config
	Metadata   Metadata
	HTTPClient *http.Client
	Log        *slog.Logger
	Failures   Counter
	Blocked    Counter
}

// New constructs an identity service.
func New(options Options) *Service {
	return &Service{
		Config:   options.Config,
		metadata: options.Metadata,
		http:     options.HTTPClient,
		Log:      options.Log,
		Failures: options.Failures,
		Blocked:  options.Blocked,
		authFailures: newAuthenticationFailureLimiter(
			options.Config.AuthFailureLimit,
			options.Config.AuthFailureWindow,
			options.Config.AuthLockout,
		),
		oidcVerifiers:         make(map[string]cachedOIDCVerifier),
		oidcDiscoveries:       make(map[oidcDiscoveryKey]*oidcDiscoveryFlight),
		oidcGenerations:       make(map[string]uint64),
		oidcLatestFingerprint: make(map[string]string),
	}
}

// SetConfig replaces the configuration. The failure limiter keeps its
// current limits; use ReplaceFailureLimiter to change those.
func (s *Service) SetConfig(cfg config.Config) {
	s.Config = cfg
}

// SetMetadata replaces the metadata store.
func (s *Service) SetMetadata(metadata Metadata) {
	s.metadata = metadata
}

// SetHTTPClient replaces the outbound HTTP client used for OIDC discovery
// and token exchange.
func (s *Service) SetHTTPClient(client *http.Client) {
	s.http = client
}

// SetLogger replaces the logger.
func (s *Service) SetLogger(log *slog.Logger) {
	s.Log = log
}

func (s *Service) meta() Metadata {
	return s.metadata
}

func (s *Service) client() *http.Client {
	return s.http
}

func (s *Service) requestLogger(r *http.Request) *slog.Logger {
	return httpx.RequestLogger(s.Log, r)
}

func (s *Service) incBlocked() {
	if s.Blocked != nil {
		s.Blocked.Inc()
	}
}

func (s *Service) incFailures() {
	if s.Failures != nil {
		s.Failures.Inc()
	}
}

// ReplaceFailureLimiter installs a limiter with the given thresholds.
// Tests use this to exercise lockout without reconstructing the compositor.
func (s *Service) ReplaceFailureLimiter(limit int, window, lockout time.Duration) {
	s.authFailures = newAuthenticationFailureLimiter(limit, window, lockout)
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
