package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

const (
	maximumMetricRepositories  = 100
	aggregateRefreshInterval   = 30 * time.Second
	overflowRepositoryLabel    = "overflow"
	unknownMetricLabel         = "unknown"
	cleanupSchedulerMetricRole = "cleanup_scheduler"
	gcSchedulerMetricRole      = "gc_scheduler"
	verifySchedulerMetricRole  = "verify_scheduler"
	migrateSchedulerMetricRole = "migrate_scheduler"
	provisioningMetricRole     = "provisioning"
)

type serverMetrics struct {
	registry *prometheus.Registry
	handler  http.Handler

	repositories *boundedLabelValues

	httpRequests           *prometheus.CounterVec
	httpDuration           *prometheus.HistogramVec
	httpErrors             prometheus.Counter
	authenticationFailures prometheus.Counter
	authenticationBlocked  prometheus.Counter
	proxyCacheRequests     *prometheus.CounterVec
	blobOperations         *prometheus.CounterVec
	blobVerifyFindings     *prometheus.GaugeVec
	blobMigration          *prometheus.CounterVec
	cleanupDeletions       *prometheus.CounterVec
	garbageCollected       prometheus.Counter
	webhookDeliveries      *prometheus.CounterVec
	provenanceChecks       *prometheus.CounterVec
	leaderLeaseExpiry      map[string]*atomic.Int64
	leaderLastHeld         *prometheus.GaugeVec
	aggregateRefreshFail   prometheus.Counter

	repositoriesGauge prometheus.Gauge
	assetsGauge       prometheus.Gauge
	uniqueBlobsGauge  prometheus.Gauge
	blobBytesGauge    prometheus.Gauge
	blobStoreBytes    *prometheus.GaugeVec
	webhookQueueGauge prometheus.Gauge
	webhookDeadGauge  prometheus.Gauge
}

func newServerMetrics(metadata store.MetricsMetadata, started time.Time) *serverMetrics {
	metrics := &serverMetrics{
		registry:     prometheus.NewRegistry(),
		repositories: newBoundedLabelValues(maximumMetricRepositories),
		httpRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "http",
				Name:      "requests_total",
				Help:      "Total HTTP requests handled by repository, format, method, and status.",
			},
			[]string{"repository", "format", "method", "status"},
		),
		httpDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "suxen",
				Subsystem: "http",
				Name:      "request_duration_seconds",
				Help:      "HTTP request latency by repository, format, method, and status.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"repository", "format", "method", "status"},
		),
		httpErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "suxen",
			Subsystem: "http",
			Name:      "errors_total",
			Help:      "Total HTTP responses with a 4xx or 5xx status.",
		}),
		authenticationFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "suxen",
			Subsystem: "authentication",
			Name:      "failures_total",
			Help:      "Credential verification failures observed by this replica.",
		}),
		authenticationBlocked: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "suxen",
			Subsystem: "authentication",
			Name:      "blocked_total",
			Help:      "Credential verifications skipped while a source is locked out.",
		}),
		proxyCacheRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "proxy",
				Name:      "cache_requests_total",
				Help:      "Proxy cache lookups split by hit or miss result.",
			},
			[]string{"repository", "format", "result"},
		),
		blobOperations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "blob",
				Name:      "operations_total",
				Help:      "Blob store operations split by operation and result (success, not_found, error).",
			},
			[]string{"operation", "result"},
		),
		blobVerifyFindings: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "suxen",
				Subsystem: "blob",
				Name:      "verify_findings",
				Help: "Findings from the last full blob-store verify run by kind " +
					"(dangling, orphaned, mismatched).",
			},
			[]string{"kind"},
		),
		blobMigration: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "blob",
				Name:      "migration_operations_total",
				Help:      "Blobs moved by blob-store migration split by operation (copied, deleted).",
			},
			[]string{"operation"},
		),
		cleanupDeletions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "cleanup",
				Name:      "deletions_total",
				Help:      "Metadata entries deleted by applied cleanup tasks.",
			},
			[]string{"repository", "kind"},
		),
		garbageCollected: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "suxen",
			Subsystem: "gc",
			Name:      "deleted_blobs_total",
			Help:      "Unreferenced blobs deleted by applied garbage collection.",
		}),
		webhookDeliveries: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "webhook",
				Name:      "deliveries_total",
				Help:      "Webhook delivery attempts split by terminal or retry outcome.",
			},
			[]string{"result"},
		),
		provenanceChecks: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "suxen",
				Subsystem: "provenance",
				Name:      "verifications_total",
				Help:      "Provenance verification attempts split by result.",
			},
			[]string{"result"},
		),
		leaderLeaseExpiry: map[string]*atomic.Int64{
			cleanupSchedulerMetricRole: new(atomic.Int64),
			gcSchedulerMetricRole:      new(atomic.Int64),
			verifySchedulerMetricRole:  new(atomic.Int64),
			migrateSchedulerMetricRole: new(atomic.Int64),
			provisioningMetricRole:     new(atomic.Int64),
		},
		leaderLastHeld: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "suxen",
				Name:      "leader_last_held_timestamp_seconds",
				Help:      "Unix timestamp when this replica most recently acquired or renewed a leader role.",
			},
			[]string{"role"},
		),
		aggregateRefreshFail: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "suxen",
			Subsystem: "metrics",
			Name:      "aggregate_refresh_failures_total",
			Help:      "Failed metadata aggregate refresh attempts.",
		}),
		repositoriesGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "repositories",
			Help:      "Repositories observed during the latest successful aggregate refresh.",
		}),
		assetsGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "assets",
			Help:      "Assets observed during the latest successful aggregate refresh.",
		}),
		uniqueBlobsGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "unique_blobs",
			Help:      "Unique blobs observed during the latest successful aggregate refresh.",
		}),
		blobBytesGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "blob_bytes",
			Help:      "Referenced blob bytes observed during the latest successful aggregate refresh.",
		}),
		blobStoreBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "blob_store_blob_bytes",
			Help:      "Deduplicated blob bytes per blob store at the latest successful aggregate refresh.",
		}, []string{"blob_store"}),
		webhookQueueGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Subsystem: "webhook",
			Name:      "queue",
			Help:      "Webhook deliveries waiting, retrying, or leased at the latest refresh.",
		}),
		webhookDeadGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "suxen",
			Subsystem: "webhook",
			Name:      "dead_letters",
			Help:      "Webhook dead letters observed during the latest aggregate refresh.",
		}),
	}

	uptime := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "suxen",
			Name:      "uptime_seconds",
			Help:      "Seconds since this server instance was constructed.",
		},
		func() float64 {
			return time.Since(started).Seconds()
		},
	)

	metrics.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.httpRequests,
		metrics.httpDuration,
		metrics.httpErrors,
		metrics.authenticationFailures,
		metrics.authenticationBlocked,
		metrics.proxyCacheRequests,
		metrics.blobOperations,
		metrics.blobVerifyFindings,
		metrics.blobMigration,
		metrics.cleanupDeletions,
		metrics.garbageCollected,
		metrics.webhookDeliveries,
		metrics.provenanceChecks,
		metrics.leaderLastHeld,
		metrics.aggregateRefreshFail,
		metrics.repositoriesGauge,
		metrics.assetsGauge,
		metrics.uniqueBlobsGauge,
		metrics.blobBytesGauge,
		metrics.blobStoreBytes,
		metrics.webhookQueueGauge,
		metrics.webhookDeadGauge,
		uptime,
		newDatabasePoolCollector(metadata),
	)
	for _, role := range []string{
		cleanupSchedulerMetricRole,
		gcSchedulerMetricRole,
		verifySchedulerMetricRole,
		migrateSchedulerMetricRole,
		provisioningMetricRole,
	} {
		metrics.registry.MustRegister(metrics.leaderCollector(role))
	}

	metrics.initializeBoundedSeries()
	metrics.handler = promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
	return metrics
}

func (metrics *serverMetrics) initializeBoundedSeries() {
	for _, result := range []string{"hit", "miss"} {
		metrics.proxyCacheRequests.WithLabelValues(
			httpx.NoRepositoryMetricLabel,
			unknownMetricLabel,
			result,
		).Add(0)
	}
	for _, operation := range []string{"put", "get", "head", "list", "delete", "ready"} {
		for _, result := range []string{"success", "error"} {
			metrics.blobOperations.WithLabelValues(operation, result).Add(0)
		}
	}
	// Only lookups distinguish a missing blob; pre-declaring not_found on the
	// read operations keeps a dangling-reference dashboard non-empty at zero.
	for _, operation := range []string{"get", "head"} {
		metrics.blobOperations.WithLabelValues(operation, "not_found").Add(0)
	}
	for _, kind := range []string{"dangling", "orphaned", "mismatched"} {
		metrics.blobVerifyFindings.WithLabelValues(kind).Set(0)
	}
	for _, operation := range []string{"copied", "deleted"} {
		metrics.blobMigration.WithLabelValues(operation).Add(0)
	}
	for _, result := range []string{"delivered", "retried", "failed"} {
		metrics.webhookDeliveries.WithLabelValues(result).Add(0)
	}
	for _, result := range []string{"passed", "failed"} {
		metrics.provenanceChecks.WithLabelValues(result).Add(0)
	}
}

func (metrics *serverMetrics) leaderCollector(role string) prometheus.Collector {
	expiry := metrics.leaderLeaseExpiry[role]
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   "suxen",
			Name:        "leader",
			Help:        "Whether this scrape target currently holds an unexpired bounded leader role.",
			ConstLabels: prometheus.Labels{"role": role},
		},
		func() float64 {
			if expiry.Load() > time.Now().UnixNano() {
				return 1
			}
			return 0
		},
	)
}

func (metrics *serverMetrics) observeHTTPRequest(
	repository string,
	format string,
	method string,
	status int,
	duration time.Duration,
) {
	repository = metrics.repositoryLabel(repository)
	format = metricFormat(format)
	method = metricMethod(method)
	statusLabel := metricStatus(status)
	labels := []string{repository, format, method, statusLabel}
	metrics.httpRequests.WithLabelValues(labels...).Inc()
	metrics.httpDuration.WithLabelValues(labels...).Observe(duration.Seconds())
	if status >= http.StatusBadRequest {
		metrics.httpErrors.Inc()
	}
}

func (metrics *serverMetrics) observeProxyCache(repository string, format string, result string) {
	metrics.ObserveProxyCache(repository, format, result)
}

// ObserveProxyCache implements content.Metrics.
func (metrics *serverMetrics) ObserveProxyCache(repository string, format string, result string) {
	if result != "hit" && result != "miss" {
		result = unknownMetricLabel
	}
	metrics.proxyCacheRequests.WithLabelValues(
		metrics.repositoryLabel(repository),
		metricFormat(format),
		result,
	).Inc()
}

func (metrics *serverMetrics) observeBlobOperation(operation string, err error) {
	metrics.ObserveBlobOperation(operation, err)
}

// ObserveBlobOperation implements content.Metrics.
//
// A missing blob is counted as "not_found" rather than "error" so a blob the
// metadata still references but the store no longer holds — a dangling
// reference — is distinguishable from an I/O or unreachable-store failure.
func (metrics *serverMetrics) ObserveBlobOperation(operation string, err error) {
	result := "success"
	switch {
	case errors.Is(err, domain.ErrNotFound):
		result = "not_found"
	case err != nil:
		result = "error"
	}
	metrics.blobOperations.WithLabelValues(operation, result).Inc()
}

func (metrics *serverMetrics) addCleanupDeletions(
	repository string,
	assets int,
	aliases int64,
) {
	repository = metrics.repositoryLabel(repository)
	metrics.cleanupDeletions.WithLabelValues(repository, "asset").Add(float64(assets))
	metrics.cleanupDeletions.WithLabelValues(repository, "alias").Add(float64(aliases))
}

func (metrics *serverMetrics) addGarbageCollected(deleted int) {
	metrics.garbageCollected.Add(float64(deleted))
}

// setVerifyFindings publishes the latest full-scope verify counts. It is a
// gauge, not a counter: the same dangling reference persists across runs until
// reconciled, so the current level — not an accumulating total — is what an
// operator alerts on.
func (metrics *serverMetrics) setVerifyFindings(dangling, orphaned, mismatched int) {
	metrics.blobVerifyFindings.WithLabelValues("dangling").Set(float64(dangling))
	metrics.blobVerifyFindings.WithLabelValues("orphaned").Set(float64(orphaned))
	metrics.blobVerifyFindings.WithLabelValues("mismatched").Set(float64(mismatched))
}

func (metrics *serverMetrics) addBlobMigration(copied, deleted int) {
	if copied > 0 {
		metrics.blobMigration.WithLabelValues("copied").Add(float64(copied))
	}
	if deleted > 0 {
		metrics.blobMigration.WithLabelValues("deleted").Add(float64(deleted))
	}
}

func (metrics *serverMetrics) observeWebhookDelivery(status string) {
	metrics.ObserveWebhookDelivery(status)
}

// ObserveWebhookDelivery implements content.Metrics.
func (metrics *serverMetrics) ObserveWebhookDelivery(status string) {
	result := map[string]string{
		"delivered": "delivered",
		"retry":     "retried",
		"dead":      "failed",
	}[status]
	if result == "" {
		result = "failed"
	}
	metrics.webhookDeliveries.WithLabelValues(result).Inc()
}

func (metrics *serverMetrics) observeProvenance(passed bool) {
	metrics.ObserveProvenance(passed)
}

// ObserveProvenance implements content.Metrics.
func (metrics *serverMetrics) ObserveProvenance(passed bool) {
	result := "failed"
	if passed {
		result = "passed"
	}
	metrics.provenanceChecks.WithLabelValues(result).Inc()
}

func (metrics *serverMetrics) holdLeaderUntil(role string, expiresAt time.Time) {
	expiry, supported := metrics.leaderLeaseExpiry[role]
	if !supported {
		return
	}
	expiry.Store(expiresAt.UnixNano())
	metrics.leaderLastHeld.WithLabelValues(role).Set(float64(time.Now().Unix()))
}

func (metrics *serverMetrics) releaseLeader(role string) {
	expiry, supported := metrics.leaderLeaseExpiry[role]
	if supported {
		expiry.Store(0)
	}
}

func (metrics *serverMetrics) refreshAggregates(
	ctx context.Context,
	metadata store.MetricsMetadata,
) error {
	stats, err := metadata.Stats(ctx)
	if err != nil {
		metrics.aggregateRefreshFail.Inc()
		return err
	}
	usage, err := metadata.StorageUsage(ctx)
	if err != nil {
		metrics.aggregateRefreshFail.Inc()
		return err
	}
	metrics.repositoriesGauge.Set(float64(stats.Repositories))
	metrics.assetsGauge.Set(float64(stats.Assets))
	metrics.uniqueBlobsGauge.Set(float64(stats.UniqueBlobs))
	metrics.blobBytesGauge.Set(float64(stats.Bytes))
	metrics.webhookQueueGauge.Set(float64(stats.WebhookQueue))
	metrics.webhookDeadGauge.Set(float64(stats.WebhookDead))
	// Per-blob-store only: blob stores are few, so this label stays bounded.
	// Per-repository usage would be unbounded, so it is served on demand by the
	// usage endpoint instead. Reset first so a store that disappeared between
	// refreshes stops reporting a stale series.
	metrics.blobStoreBytes.Reset()
	for _, blobStore := range usage.BlobStores {
		metrics.blobStoreBytes.WithLabelValues(blobStore.BlobStore).Set(float64(blobStore.Bytes))
	}
	return nil
}

func (metrics *serverMetrics) repositoryLabel(repository string) string {
	if repository == "" || repository == httpx.NoRepositoryMetricLabel {
		return httpx.NoRepositoryMetricLabel
	}
	return metrics.repositories.value(repository, overflowRepositoryLabel)
}

// metricFormat keeps the format label bounded: the compile-time format
// registry is the closed vocabulary, so plugin formats get their own label
// without opening the cardinality to request data.
func metricFormat(format string) string {
	if format == httpx.ControlPlaneMetricFormat || spiformat.Registered(format) {
		return format
	}
	return unknownMetricLabel
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions:
		return method
	default:
		return unknownMetricLabel
	}
}

func metricStatus(status int) string {
	if status < 100 || status > 599 {
		return unknownMetricLabel
	}
	return strconv.Itoa(status)
}

type boundedLabelValues struct {
	mu      sync.Mutex
	limit   int
	allowed map[string]struct{}
}

func newBoundedLabelValues(limit int) *boundedLabelValues {
	return &boundedLabelValues{
		limit:   limit,
		allowed: make(map[string]struct{}, limit),
	}
}

func (values *boundedLabelValues) value(candidate string, overflow string) string {
	values.mu.Lock()
	defer values.mu.Unlock()
	if _, found := values.allowed[candidate]; found {
		return candidate
	}
	if len(values.allowed) >= values.limit {
		return overflow
	}
	values.allowed[candidate] = struct{}{}
	return candidate
}

type databasePoolCollector struct {
	metadata store.MetricsMetadata
	desc     map[string]*prometheus.Desc
}

func newDatabasePoolCollector(metadata store.MetricsMetadata) *databasePoolCollector {
	definitions := map[string]string{
		"max_open_connections":        "Maximum number of open database connections.",
		"open_connections":            "Current number of open database connections.",
		"in_use_connections":          "Current number of database connections in use.",
		"idle_connections":            "Current number of idle database connections.",
		"wait_count_total":            "Total database connection waits.",
		"wait_duration_seconds_total": "Total time blocked waiting for database connections.",
	}
	descriptions := make(map[string]*prometheus.Desc, len(definitions))
	for name, help := range definitions {
		descriptions[name] = prometheus.NewDesc(
			prometheus.BuildFQName("suxen", "database_pool", name),
			help,
			[]string{"backend"},
			nil,
		)
	}
	return &databasePoolCollector{metadata: metadata, desc: descriptions}
}

func (collector *databasePoolCollector) Describe(channel chan<- *prometheus.Desc) {
	for _, description := range collector.desc {
		channel <- description
	}
}

func (collector *databasePoolCollector) Collect(channel chan<- prometheus.Metric) {
	provider, supported := collector.metadata.(store.DatabasePoolStatsProvider)
	if !supported {
		return
	}
	pool := provider.DatabasePoolStats()
	backend := pool.Backend
	if backend != "sqlite" && backend != "postgres" {
		backend = unknownMetricLabel
	}
	stats := pool.Stats
	collector.sendGauge(channel, "max_open_connections", float64(stats.MaxOpenConnections), backend)
	collector.sendGauge(channel, "open_connections", float64(stats.OpenConnections), backend)
	collector.sendGauge(channel, "in_use_connections", float64(stats.InUse), backend)
	collector.sendGauge(channel, "idle_connections", float64(stats.Idle), backend)
	channel <- prometheus.MustNewConstMetric(
		collector.desc["wait_count_total"],
		prometheus.CounterValue,
		float64(stats.WaitCount),
		backend,
	)
	channel <- prometheus.MustNewConstMetric(
		collector.desc["wait_duration_seconds_total"],
		prometheus.CounterValue,
		stats.WaitDuration.Seconds(),
		backend,
	)
}

func (collector *databasePoolCollector) sendGauge(
	channel chan<- prometheus.Metric,
	name string,
	value float64,
	backend string,
) {
	description, found := collector.desc[name]
	if !found {
		panic(fmt.Sprintf("unknown database pool metric %q", name))
	}
	channel <- prometheus.MustNewConstMetric(
		description,
		prometheus.GaugeValue,
		value,
		backend,
	)
}
