// Package content is the shared repository data plane: blob-store opening,
// uploads, proxy fetch, asset serving, provenance, and webhook enqueue.
// OCI and raw HTTP handlers depend on this package instead of importing server.
package content

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/store"
)

// Metrics is the subset of process metrics the data plane records.
type Metrics interface {
	ObserveProxyCache(repository string, format string, result string)
	ObserveBlobOperation(operation string, err error)
	ObserveProvenance(passed bool)
	ObserveWebhookDelivery(status string)
}

// Runtime is the data plane's dependency set. It owns its dependencies; the
// compositor hands them over once in Options and pushes later changes
// through the Set methods. It does not point back at Server.
type Runtime struct {
	Config           config.Config
	Log              *slog.Logger
	Metrics          Metrics
	Auth             *identity.Service
	BlobStores       *StoreManager
	UserAgent        string
	WebhookUserAgent string

	metadata        store.Store
	http            *http.Client
	webhookHTTP     *http.Client
	background      *sync.WaitGroup
	schedulerID     string
	uploadSlots     chan struct{}
	largeIndexSlots chan struct{}
	stagingMu       sync.Mutex
	stagingFiles    map[string]*stagingFile
}

// Options are the dependencies of a Runtime.
type Options struct {
	Config            config.Config
	Metadata          store.Store
	DefaultBlobStore  blob.Store
	HTTPClient        *http.Client
	WebhookHTTPClient *http.Client
	Log               *slog.Logger
	Metrics           Metrics
	Auth              *identity.Service
	Factory           BlobStoreFactory
	// Background tracks detached goroutines so the process can drain them
	// on shutdown; a WaitGroup must be shared by pointer.
	Background  *sync.WaitGroup
	SchedulerID string
	// UserAgent and WebhookUserAgent identify upstream fetches and webhook
	// deliveries respectively.
	UserAgent        string
	WebhookUserAgent string
}

// New constructs a data-plane runtime.
func New(options Options) *Runtime {
	maximumUploads := options.Config.MaxConcurrentUploads
	if maximumUploads <= 0 {
		maximumUploads = 4
	}
	runtime := &Runtime{
		Config:           options.Config,
		Log:              options.Log,
		Metrics:          options.Metrics,
		Auth:             options.Auth,
		UserAgent:        options.UserAgent,
		WebhookUserAgent: options.WebhookUserAgent,
		metadata:         options.Metadata,
		http:             options.HTTPClient,
		webhookHTTP:      options.WebhookHTTPClient,
		background:       options.Background,
		schedulerID:      options.SchedulerID,
		uploadSlots:      make(chan struct{}, maximumUploads),
		largeIndexSlots:  make(chan struct{}, 1),
	}
	runtime.BlobStores = NewStoreManager(
		options.Metadata,
		options.DefaultBlobStore,
		options.Factory,
		options.Metrics,
	)
	return runtime
}

// AcquireUpload reserves one process-local parsing/staging slot. Cluster-wide
// storage correctness is handled by metadata leases; this budget protects each
// replica's memory, temporary disk, and decoder work.
func (rt *Runtime) AcquireUpload(ctx context.Context) (func(), error) {
	select {
	case rt.uploadSlots <- struct{}{}:
		return func() { <-rt.uploadSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SetConfig replaces the configuration.
func (rt *Runtime) SetConfig(cfg config.Config) {
	rt.Config = cfg
}

// SetMetadata replaces the metadata store, including the one the blob-store
// manager resolves resources from.
func (rt *Runtime) SetMetadata(metadata store.Store) {
	rt.metadata = metadata
	rt.BlobStores.SetMetadata(metadata)
}

// SetHTTPClient replaces the outbound HTTP clients in tests and startup wiring.
func (rt *Runtime) SetHTTPClient(client *http.Client) {
	rt.http = client
	rt.webhookHTTP = client
}

// SetLogger replaces the logger.
func (rt *Runtime) SetLogger(log *slog.Logger) {
	rt.Log = log
}

// OCIMetadata and UploadLedger hand the OCI handler only the backend capabilities
// it needs, so it never receives store.Store. They read the current backend on
// each call, not a captured copy, so a SetMetadata (startup wiring or a test
// barrier) is observed and there is one source of truth.
func (rt *Runtime) OCIMetadata() store.OCIMetadata {
	return rt.meta()
}

// OCIRepositoryView gives the OCI handler an ID-bound view of one repository built
// from the runtime's current backend, so a SetMetadata wrapper that overrides
// ID-bound methods is delegated through (a view bound to the raw store would
// bypass those overrides).
func (rt *Runtime) OCIRepositoryView(repository domain.Repository) store.RepositoryView {
	return rt.metaFor(repository)
}

func (rt *Runtime) UploadLedger() store.UploadLedger {
	return rt.meta()
}

func (rt *Runtime) RequestLogger(r *http.Request) *slog.Logger {
	return rt.requestLogger(r)
}

func (rt *Runtime) meta() store.Store {
	return rt.metadata
}

func (rt *Runtime) metaFor(repository domain.Repository) store.RepositoryView {
	return rt.meta().ForRepository(repository)
}

func (rt *Runtime) client() *http.Client {
	return rt.http
}

func (rt *Runtime) webhookClient() *http.Client {
	if rt.webhookHTTP != nil {
		return rt.webhookHTTP
	}
	return rt.http
}

func (rt *Runtime) requestLogger(r *http.Request) *slog.Logger {
	return httpx.RequestLogger(rt.Log, r)
}

func (rt *Runtime) goBackground(fn func()) {
	rt.background.Add(1)
	go func() {
		defer rt.background.Done()
		fn()
	}()
}

func (rt *Runtime) leaderID() string {
	return rt.schedulerID
}

func (rt *Runtime) observeProxyCache(repository string, format string, result string) {
	if rt.Metrics != nil {
		rt.Metrics.ObserveProxyCache(repository, format, result)
	}
}

func (rt *Runtime) observeProvenance(passed bool) {
	if rt.Metrics != nil {
		rt.Metrics.ObserveProvenance(passed)
	}
}

func (rt *Runtime) observeWebhookDelivery(status string) {
	if rt.Metrics != nil {
		rt.Metrics.ObserveWebhookDelivery(status)
	}
}
