package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/oci"
	"github.com/suxen-project/suxen/internal/outbound"
	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/startup"
	"github.com/suxen-project/suxen/internal/store"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// BlobStoreFactory opens a blob store for a driver name and configuration.
// Custom distributions pass one to NewWithBlobStoreFactory.
type BlobStoreFactory = content.BlobStoreFactory

// Version identifies this build. Release builds replace "dev" through linker flags.
var Version = "dev"

// Server implements the suxen control plane and repository data plane.
type Server struct {
	cfg             config.Config
	metadata        store.Store
	accounts        accountCommands
	oidc            oidcCommands
	roles           roleCommands
	repositories    repositoryCommands
	cleanupPolicies cleanupPolicyCommands
	trustPolicies   trustPolicyCommands
	classifications classificationCommands
	downloadGates   downloadGateCommands
	webhooks        webhookCommands
	// blobStoreCommands commits the metadata half of a blob-store mutation and its
	// ownership record; the controller and HTTP handlers do the physical backend
	// work around it.
	blobStoreCommands blobStoreCommands
	blobs             blob.Store
	blobStores        *content.StoreManager
	log               *slog.Logger
	identity          *identity.Service
	content           *content.Runtime
	oci               *oci.Handler
	// coreWire holds the wire protocols of formats compiled into the server
	// core, consulted after the plugin registry by the same dispatch code.
	coreWire       map[string]spiformat.WireProtocol
	schedulerOnce  sync.Once
	schedulerID    string
	metrics        *serverMetrics
	extraListeners extraHTTPListeners
	extraErrors    chan error
	httpHandler    http.Handler
	// initializing gates readiness while startup work (migrations, provisioning,
	// operational validation) is still running or retrying. Liveness stays up so
	// a slow or unavailable external dependency never restarts the pod; readiness
	// simply reports 503 until initialization finishes. Default (unset) is ready,
	// so library callers and tests that skip the boot sequence are unaffected.
	initializing atomic.Bool
	// backgroundTasks tracks detached best-effort goroutines (e.g. async
	// last-download bookkeeping) so Close can drain them before shutdown.
	backgroundTasks sync.WaitGroup
}

// New constructs an HTTP server over the supplied metadata and blob stores.
func New(
	cfg config.Config,
	metadata store.Store,
	blobs blob.Store,
	log *slog.Logger,
) *Server {
	return NewWithBlobStoreFactory(
		cfg,
		metadata,
		blobs,
		content.RegistryFactory,
		log,
	)
}

// NewWithBlobStoreFactory constructs a server with an explicit compiled-in blob driver
// factory. Most callers should use New; this constructor supports custom distributions
// and tests that need to select named drivers deterministically.
func NewWithBlobStoreFactory(
	cfg config.Config,
	metadata store.Store,
	blobs blob.Store,
	factory BlobStoreFactory,
	log *slog.Logger,
) *Server {
	outboundPolicy := outbound.NewPolicy(outbound.Options{
		AllowedCIDRs: cfg.OutboundCIDRs,
		AllowedHosts: cfg.OutboundHosts,
	})
	started := time.Now()
	server := &Server{
		cfg:               cfg,
		metadata:          metadata,
		accounts:          controlplane.NewAccountService(metadata),
		oidc:              controlplane.NewOIDCService(metadata),
		roles:             controlplane.NewRoleService(metadata),
		repositories:      controlplane.NewRepositoryService(metadata),
		cleanupPolicies:   controlplane.NewCleanupPolicyService(metadata),
		trustPolicies:     controlplane.NewTrustPolicyService(metadata),
		classifications:   controlplane.NewClassificationService(metadata),
		downloadGates:     controlplane.NewDownloadGateService(metadata),
		webhooks:          controlplane.NewWebhookService(metadata),
		blobStoreCommands: controlplane.NewBlobStoreService(metadata),
		blobs:             blobs,
		log:               log,
		schedulerID:       randomSecret(12),
		extraErrors:       make(chan error, 1),
	}
	controlClient := outboundPolicy.Client(cfg.OutboundTimeout)
	streamingClient := outboundPolicy.StreamingClient(cfg.ProxyResponseHeaderTimeout)
	server.metrics = newServerMetrics(metadata, started)
	server.identity = identity.New(identity.Options{
		Config:     cfg,
		Metadata:   metadata,
		HTTPClient: controlClient,
		Log:        log,
		Failures:   server.metrics.authenticationFailures,
		Blocked:    server.metrics.authenticationBlocked,
	})
	server.content = content.New(content.Options{
		Config:            cfg,
		Metadata:          metadata,
		DefaultBlobStore:  blobs,
		HTTPClient:        streamingClient,
		WebhookHTTPClient: controlClient,
		Log:               log,
		Metrics:           server.metrics,
		Auth:              server.identity,
		Factory:           factory,
		Background:        &server.backgroundTasks,
		SchedulerID:       server.schedulerID,
		UserAgent:         "suxen/" + Version,
		WebhookUserAgent:  "suxen-webhooks/" + Version,
	})
	server.blobStores = server.content.BlobStores
	server.oci = oci.New(server.content)
	server.coreWire = map[string]spiformat.WireProtocol{"oci": oci.Format{}}
	server.httpHandler = http.HandlerFunc(server.ServeHTTP)
	return server
}

// goBackground runs fn in a tracked goroutine so Close can wait for detached
// best-effort work (last-download bookkeeping, webhook projection) to finish.
// This keeps such writes from racing test temp-dir cleanup after a request
// whose context was intentionally detached.
func (s *Server) goBackground(fn func()) {
	s.backgroundTasks.Add(1)
	go func() {
		defer s.backgroundTasks.Done()
		fn()
	}()
}

// Close waits for in-flight detached background tasks to finish, bounded so a
// stuck task cannot hang shutdown. It does not stop the scheduler, which is
// governed by the context passed to Start.
func (s *Server) Close() error {
	s.stopExtraListeners()
	s.content.CloseStaging()
	done := make(chan struct{})
	go func() {
		s.backgroundTasks.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// metadataLifecycle is the metadata-database lifecycle capability the server
// needs across its lifetime: apply schema migrations at bootstrap, and probe
// database connectivity from the HTTP readiness handler.
type metadataLifecycle interface {
	Migrate(context.Context) error
	Ready(context.Context) error
}

// metadataLifecycle narrows the metadata store to the database lifecycle
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) metadataLifecycle() metadataLifecycle {
	return s.metadata
}

// Bootstrap migrates the database and creates the initial repositories and admin.
// The returned message contains one-time credentials only when the admin was created.
func (s *Server) Bootstrap(ctx context.Context) (string, error) {
	if err := s.metadataLifecycle().Migrate(ctx); err != nil {
		var schema *store.SchemaError
		if !errors.As(err, &schema) {
			err = startup.Retry(err)
		}
		return "", fmt.Errorf("migrate metadata: %w", err)
	}
	if err := s.ensureDefaultBlobStore(ctx); err != nil {
		return "", fmt.Errorf("create default blob store: %w", err)
	}

	defaultReport, err := s.provisionEngine().Apply(
		ctx,
		provision.Document{APIVersion: provision.APIVersion},
		provision.Options{},
	)
	if err != nil {
		return "", fmt.Errorf("reconcile built-in desired state: %w", classifyProvisionError(err))
	}
	if defaultReport.Failed() {
		return "", fmt.Errorf("reconcile built-in desired state: %w", classifyProvisionReport(defaultReport))
	}

	password := s.cfg.BootstrapPassword
	generatedPassword := password == ""
	if password == "" {
		password = randomSecret(18)
	}

	token := s.cfg.BootstrapToken
	generatedToken := token == ""
	if token == "" {
		token = randomSecret(32)
	}

	err = s.accountStore().CreateBootstrapAdmin(ctx, s.cfg.BootstrapUser, password, token)
	if errors.Is(err, domain.ErrConflict) {
		s.refreshAggregateMetrics(ctx)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("create bootstrap administrator: %w", startup.Retry(err))
	}
	s.refreshAggregateMetrics(ctx)

	if !generatedPassword && !generatedToken {
		return "", nil
	}
	message := []string{
		fmt.Sprintf("bootstrap admin created: username=%s", s.cfg.BootstrapUser),
	}
	if generatedPassword {
		message = append(message, "password="+password)
	}
	if generatedToken {
		message = append(message, "token="+token)
	}
	return strings.Join(message, " "), nil
}

// ValidateOperationalConfiguration checks settings that depend on persisted metadata.
// Missing browser-login state is reported as a warning because OIDC bearer authentication
// remains available; metadata query failures are returned to prevent a misleading startup.
func (s *Server) ValidateOperationalConfiguration(ctx context.Context) error {
	providers, err := s.oidcReads().OIDCProviders(ctx)
	if err != nil {
		return fmt.Errorf("load OIDC providers for configuration validation: %w", startup.Retry(err))
	}
	if len(providers) > 0 && s.cfg.OIDCStateSecret == "" {
		s.log.Warn(
			"interactive OIDC login disabled",
			"reason", "SUXEN_OIDC_STATE_SECRET is not configured",
			"provider_count", len(providers),
		)
	}
	return nil
}

func (s *Server) ensureDefaultBlobStore(ctx context.Context) error {
	existing, err := s.blobStoreReads().BlobStore(ctx, "default")
	if err == nil {
		if s.blobs != nil {
			s.blobStores.Remember(existing, s.blobs)
		}
		return nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return startup.Retry(err)
	}
	if s.cfg.BlobURL == "" {
		return fmt.Errorf("SUXEN_BLOBSTORE is required to create the default blob store")
	}
	driver, err := blobStoreDriver(s.cfg.BlobURL)
	if err != nil {
		return err
	}
	identity, err := content.PhysicalIdentity(driver, s.cfg.BlobURL)
	if err != nil {
		return err
	}
	configured := domain.BlobStore{
		Name:   "default",
		Driver: driver,
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_BLOBSTORE",
		},
		PhysicalIdentity: identity,
	}
	if err := s.blobStoreAdmin().CreateBlobStore(ctx, configured); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return startup.Retry(err)
		}
		existing, err = s.blobStoreReads().BlobStore(ctx, "default")
		if err != nil {
			return startup.Retry(err)
		}
		configured = existing
	}
	if s.blobs != nil {
		s.blobStores.Remember(configured, s.blobs)
	}
	return nil
}

func blobStoreDriver(configuration string) (string, error) {
	scheme, _, found := strings.Cut(configuration, "://")
	if !found || scheme == "" {
		return "", fmt.Errorf("blob store configuration must include a driver scheme")
	}
	return scheme, nil
}

func randomSecret(size int) string {
	return httpx.RandomSecret(size)
}

// ServeHTTP dispatches operational, administrative, Raw, and OCI requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestContext := &httpx.RequestLog{
		RequestID: httpx.RequestID(r.Header.Values(httpx.RequestIDHeader)),
		Subject:   "anonymous",
		Format:    httpx.ControlPlaneMetricFormat,
	}
	r = httpx.WithRequestLog(r, requestContext)

	tracked := httpx.NewStatusWriter(w, s.log, requestContext)
	tracked.Header().Set("X-Content-Type-Options", "nosniff")
	tracked.Header().Set(httpx.RequestIDHeader, requestContext.RequestID)

	defer func() {
		duration := time.Since(started)
		s.metrics.observeHTTPRequest(
			tracked.MetricRepository(),
			tracked.MetricFormat(),
			r.Method,
			tracked.Status(),
			duration,
		)
		if !quietAccessLogPath(r.URL.Path) {
			attributes := tracked.RequestLogAttributes()
			attributes = append(
				attributes,
				"method", r.Method,
				"path", r.URL.Path,
				"status", tracked.Status(),
				"duration", duration.String(),
			)
			s.log.Info("request", attributes...)
		}
	}()

	if !s.identity.ProtectCookieAuthenticatedMutation(tracked, r) {
		return
	}
	s.route(tracked, r)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		if !httpx.AllowReadOnlyMethod(w, r) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.URL.Path == "/readyz":
		s.handleReadiness(w, r)
	case r.URL.Path == "/metrics":
		s.handleMetrics(w, r)
	case r.URL.Path == "/version":
		if !httpx.AllowReadOnlyMethod(w, r) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"version": Version})
	case r.URL.Path == "/api/openapi.json":
		s.handleOpenAPI(w, r)
	case s.servesUI() && isUIPath(r.URL.Path):
		s.handleUI(w, r)
	case strings.HasPrefix(r.URL.Path, "/auth/oidc/"):
		s.identity.HandleOIDCLogin(w, r)
	case r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/"):
		s.handleDefaultOCIRepository(w, r)
	case r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/"):
		s.handleAdmin(w, r)
	case strings.HasPrefix(r.URL.Path, "/repository/"):
		s.handleRepository(w, r)
	default:
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) servesUI() bool {
	return uiEnabled && !s.cfg.DisableUI
}

func isUIPath(requestPath string) bool {
	return requestPath == "/" || requestPath == "/ui" || strings.HasPrefix(requestPath, "/ui/")
}

func (s *Server) handleDefaultOCIRepository(w http.ResponseWriter, r *http.Request) {
	httpx.SelectErrorResponseFormat(w, httpx.ErrorResponseFormatOCI)
	repository, err := s.resolveOCIRootRepository(r)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.SetRequestMetricLabels(w, repository.Name, repository.Format)
	// The root binding is the repository-relative v2 route without its
	// /repository/{name} prefix, so it dispatches through the same hook. The
	// ping (/v2/) authenticates inside the handler: an anonymous 200 would
	// make Docker drop its stored credentials, so it must not be gated by the
	// generic privilege check here.
	requestPath := "v2" + strings.TrimPrefix(r.URL.Path, "/v2")
	if requestPath == "v2/" || requestPath == "v2" {
		if s.rootOCIPing(w, r, repository, requestPath) {
			return
		}
	}
	if s.serveWireProtocol(w, r, repository, requestPath) {
		return
	}
	httpx.MethodNotAllowed(
		w,
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPatch,
		http.MethodPut,
		http.MethodDelete,
	)
}

// rootOCIPing serves the bound-root /v2/ ping without the host privilege
// check, exactly as before the wire dispatch; the handler challenges
// unauthenticated clients itself.
func (s *Server) rootOCIPing(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	requestPath string,
) bool {
	wire, ok := s.wireProtocol(repository.Format)
	if !ok {
		return false
	}
	if preparer, ok := wire.(wireResponsePreparer); ok {
		preparer.PrepareWireResponse(w, requestPath)
	}
	wire.ServeWire(w, r, repository.FormatView(), requestPath, s.content.WireTools(repository))
	return true
}

func (s *Server) requestLogger(r *http.Request) *slog.Logger {
	return httpx.RequestLogger(s.log, r)
}

func quietAccessLogPath(requestPath string) bool {
	switch requestPath {
	case "/healthz", "/readyz", "/metrics":
		return true
	default:
		return false
	}
}

// SetInitializing controls the readiness gate. While true, /readyz reports 503
// so the pod stays out of rotation until startup work completes; /healthz is
// unaffected, so the process is never restarted for an unavailable dependency.
func (s *Server) SetInitializing(initializing bool) {
	s.initializing.Store(initializing)
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if !httpx.AllowReadOnlyMethod(w, r) {
		return
	}

	if s.initializing.Load() {
		httpx.WriteProblem(
			w,
			http.StatusServiceUnavailable,
			"initializing",
			"server is still initializing",
		)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// Readiness gates only on metadata, the one dependency every request path
	// needs. Blob-store availability is deliberately not checked here: a store
	// outage affects only the requests that touch that store, and because every
	// replica shares the same external stores, failing readiness on a store blip
	// would drop all replicas at once — turning a partial, per-request
	// degradation into a total outage that also 502s unrelated traffic such as
	// registry image pulls. Store failures instead surface per request.
	if err := s.metadataLifecycle().Ready(ctx); err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusServiceUnavailable,
			"database_unavailable",
			"database unavailable",
			err,
		)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
