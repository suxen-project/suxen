package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestMetricsEndpointExposesParsedOperationalSeries(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/metrics.bin",
		[]byte("metrics payload"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	download := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/releases/metrics.bin",
		nil,
		true,
	)
	assertStatus(t, download, http.StatusOK)
	download.Body.Close()
	blobStore, err := fixture.Handler.blobStores.Store(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobStore.Head(context.Background(), strings.Repeat("0", 64)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing blob returned %v, want ErrNotFound", err)
	}

	fixture.Handler.metrics.observeWebhookDelivery("delivered")
	fixture.Handler.metrics.observeWebhookDelivery("retry")
	fixture.Handler.metrics.observeWebhookDelivery("dead")
	fixture.Handler.metrics.addCleanupDeletions("raw", 2, 1)
	fixture.Handler.metrics.addGarbageCollected(3)
	fixture.Handler.metrics.holdLeaderUntil(
		cleanupSchedulerMetricRole,
		time.Now().Add(time.Minute),
	)

	response := fixture.request(t, http.MethodGet, "/metrics", nil, true)
	assertStatus(t, response, http.StatusOK)
	families := parseMetricResponse(t, response)

	required := []string{
		"go_goroutines",
		"process_cpu_seconds_total",
		"suxen_http_requests_total",
		"suxen_http_request_duration_seconds",
		"suxen_blob_operations_total",
		"suxen_cleanup_deletions_total",
		"suxen_gc_deleted_blobs_total",
		"suxen_webhook_deliveries_total",
		"suxen_database_pool_open_connections",
		"suxen_leader",
		"suxen_repositories",
	}
	for _, name := range required {
		if families[name] == nil {
			t.Errorf("metrics response is missing %s", name)
		}
	}

	assertMetricValue(t, families, "suxen_http_requests_total", map[string]string{
		"repository": "raw",
		"format":     "raw",
		"method":     http.MethodPut,
		"status":     strconv.Itoa(http.StatusCreated),
	}, 1)
	assertHistogramCount(t, families, "suxen_http_request_duration_seconds", map[string]string{
		"repository": "raw",
		"format":     "raw",
		"method":     http.MethodPut,
		"status":     strconv.Itoa(http.StatusCreated),
	}, 1)
	assertMetricValue(t, families, "suxen_blob_operations_total", map[string]string{
		"operation": "put",
		"result":    "success",
	}, 1)
	assertMetricValue(t, families, "suxen_blob_operations_total", map[string]string{
		"operation": "get",
		"result":    "success",
	}, 1)
	assertMetricValue(t, families, "suxen_blob_operations_total", map[string]string{
		"operation": "head",
		"result":    "not_found",
	}, 1)
	assertMetricValue(t, families, "suxen_webhook_deliveries_total", map[string]string{
		"result": "delivered",
	}, 1)
	assertMetricValue(t, families, "suxen_webhook_deliveries_total", map[string]string{
		"result": "retried",
	}, 1)
	assertMetricValue(t, families, "suxen_webhook_deliveries_total", map[string]string{
		"result": "failed",
	}, 1)
	assertMetricValue(t, families, "suxen_cleanup_deletions_total", map[string]string{
		"repository": "raw",
		"kind":       "asset",
	}, 2)
	assertMetricValue(t, families, "suxen_gc_deleted_blobs_total", nil, 3)
	assertMetricValue(t, families, "suxen_database_pool_open_connections", map[string]string{
		"backend": "sqlite",
	}, -1)
	assertMetricValue(t, families, "suxen_leader", map[string]string{
		"role": cleanupSchedulerMetricRole,
	}, 1)
	assertMetricValue(t, families, "suxen_leader_last_held_timestamp_seconds", map[string]string{
		"role": cleanupSchedulerMetricRole,
	}, -1)

	for name, family := range families {
		if !strings.HasPrefix(name, "suxen_") {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "path", "user", "username", "secret", "digest":
					t.Errorf("metric %s exposes forbidden label %q", name, label.GetName())
				}
			}
		}
	}
}

func TestServeAssetMissingBlobCountsNotFound(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/releases/dangling.bin",
		[]byte("dangling payload"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	// Drop the backing blob while the asset metadata remains: a dangling
	// reference, the case blob-not-found observability must surface.
	blobStore, err := fixture.Handler.blobStores.Store(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := collectBlobs(context.Background(), blobStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected a stored blob to delete")
	}
	for _, info := range blobs {
		if err := blobStore.Delete(context.Background(), info.Digest); err != nil {
			t.Fatal(err)
		}
	}

	download := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/releases/dangling.bin",
		nil,
		true,
	)
	assertStatus(t, download, http.StatusNotFound)
	download.Body.Close()

	response := fixture.request(t, http.MethodGet, "/metrics", nil, true)
	assertStatus(t, response, http.StatusOK)
	families := parseMetricResponse(t, response)
	assertMetricValue(t, families, "suxen_blob_operations_total", map[string]string{
		"operation": "get",
		"result":    "not_found",
	}, 1)
}

func TestMetricsEndpointNegotiatesOpenMetrics(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()

	fixture.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("OpenMetrics status = %d, want 200", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(
		contentType,
		"application/openmetrics-text",
	) {
		t.Fatalf("OpenMetrics content type = %q", contentType)
	}
	if !strings.HasSuffix(response.Body.String(), "# EOF\n") {
		t.Fatal("OpenMetrics response is missing the end marker")
	}
}

func TestMetricsEndpointRequiresStatsPrivilege(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/metrics", nil, false)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics status = %d, want 401", response.StatusCode)
	}
	if strings.Contains(response.Header.Get("Content-Type"), "openmetrics") {
		t.Fatal("unauthenticated metrics request exposed an OpenMetrics response")
	}
	response.Body.Close()

	const scrapeToken = "metrics-scrape-token"
	if err := fixture.Metadata.CreateUser(
		context.Background(),
		"metrics-scraper",
		"test-password",
		false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Metadata.CreateToken(
		context.Background(),
		"metrics-scraper",
		"scrape",
		scrapeToken,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	forbidden := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/metrics",
		nil,
		"",
		scrapeToken,
	)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("unprivileged metrics status = %d, want 403", forbidden.StatusCode)
	}
	forbidden.Body.Close()

	if err := fixture.Metadata.CreateRole(context.Background(), domain.Role{
		Name:       "metrics-reader",
		Privileges: []string{"admin:stats:read"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.SetUserRoles(
		context.Background(),
		"metrics-scraper",
		[]string{"metrics-reader"},
	); err != nil {
		t.Fatal(err)
	}
	allowed := fixture.requestWithBearer(
		t,
		http.MethodGet,
		"/metrics",
		nil,
		"",
		scrapeToken,
	)
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("stats reader metrics status = %d, want 200", allowed.StatusCode)
	}
	allowed.Body.Close()
}

func TestProxyCacheMetricsCountHitsAndMisses(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name:     "metrics-proxy",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://upstream.example.test/",
	}); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("cached response")),
				Request:    request,
			}, nil
		}),
	})

	for range 2 {
		response := fixture.request(
			t,
			http.MethodGet,
			"/repository/metrics-proxy/example.bin",
			nil,
			true,
		)
		assertStatus(t, response, http.StatusOK)
		response.Body.Close()
	}

	families, err := fixture.Handler.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)
	labels := map[string]string{
		"repository": "metrics-proxy",
		"format":     "raw",
	}
	assertMetricValue(t, byName, "suxen_proxy_cache_requests_total", withLabel(labels, "result", "miss"), 1)
	assertMetricValue(t, byName, "suxen_proxy_cache_requests_total", withLabel(labels, "result", "hit"), 1)
}

func TestCleanupAndGarbageCollectionMetricsFollowAppliedWork(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	upload := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/temporary/cleanup.bin",
		[]byte("eligible for cleanup"),
		true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	policy := domain.CleanupPolicy{
		Name:         "metrics-cleanup",
		Repositories: []string{"raw"},
		Criteria: domain.CleanupCriteria{{
			Path:  "sys.path",
			Op:    "matches",
			Value: `^temporary/`,
		}},
		Action:  "delete",
		Enabled: true,
	}
	if err := fixture.Metadata.CreateCleanupPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	storedPolicy, err := fixture.Metadata.CleanupPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Handler.runCleanupTask(
		context.Background(),
		storedPolicy,
		"raw",
		false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Handler.runGarbageCollection(
		context.Background(),
		false,
		0,
		"",
	); err != nil {
		t.Fatal(err)
	}

	families, err := fixture.Handler.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)
	assertMetricValue(t, byName, "suxen_cleanup_deletions_total", map[string]string{
		"repository": "raw",
		"kind":       "asset",
	}, 1)
	assertMetricValue(t, byName, "suxen_gc_deleted_blobs_total", nil, 1)
}

func TestMetricsScrapesDoNotRefreshMetadataAggregates(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	metadata := &controlledStatsStore{
		Store: fixture.Metadata,
		stats: store.Stats{
			Repositories: 7,
			Assets:       11,
		},
	}
	handler := New(
		config.Config{},
		metadata,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	handler.refreshAggregateMetrics(context.Background())
	metadata.err = errors.New("aggregate query unavailable")
	handler.refreshAggregateMetrics(context.Background())
	if metadata.calls != 2 {
		t.Fatalf("aggregate refresh calls = %d, want 2", metadata.calls)
	}

	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("metrics status = %d, want 200; body: %s", response.Code, response.Body.String())
		}
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(strings.NewReader(response.Body.String()))
		if err != nil {
			t.Fatalf("parse metrics after aggregate failure: %v", err)
		}
		assertMetricValue(t, families, "suxen_repositories", nil, 7)
		assertMetricValue(t, families, "suxen_metrics_aggregate_refresh_failures_total", nil, 1)
	}
	if metadata.calls != 2 {
		t.Fatalf("scraping called Stats; aggregate calls = %d, want 2", metadata.calls)
	}
}

func TestRepositoryMetricLabelsHaveAHardCardinalityLimit(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	metrics := fixture.Handler.metrics
	for index := 0; index < maximumMetricRepositories+10; index++ {
		metrics.observeHTTPRequest(
			"repository-"+strconv.Itoa(index),
			"raw",
			http.MethodGet,
			http.StatusOK,
			time.Millisecond,
		)
	}
	metrics.observeHTTPRequest("repository-0", "untrusted-format", "TRACE", 999, time.Millisecond)

	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)
	requests := byName["suxen_http_requests_total"]
	if requests == nil {
		t.Fatal("request metric was not gathered")
	}
	if got, want := len(requests.Metric), maximumMetricRepositories+2; got != want {
		t.Fatalf("request series = %d, want %d bounded series", got, want)
	}
	assertMetricValue(t, byName, "suxen_http_requests_total", map[string]string{
		"repository": overflowRepositoryLabel,
		"format":     "raw",
		"method":     http.MethodGet,
		"status":     strconv.Itoa(http.StatusOK),
	}, 10)
	assertMetricValue(t, byName, "suxen_http_requests_total", map[string]string{
		"repository": "repository-0",
		"format":     unknownMetricLabel,
		"method":     unknownMetricLabel,
		"status":     unknownMetricLabel,
	}, 1)
}

func TestLeaderMetricsFollowActualLeaseValidityAndTakeover(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	const leaseDuration = 500 * time.Millisecond
	acquired, err := fixture.Handler.acquireSchedulerLease(
		ctx,
		cleanupLeaseName,
		cleanupSchedulerMetricRole,
		leaseDuration,
	)
	if err != nil || !acquired {
		t.Fatalf("acquire first cleanup lease: acquired=%v err=%v", acquired, err)
	}
	firstFamilies, err := fixture.Handler.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	assertMetricValue(
		t,
		metricFamiliesByName(firstFamilies),
		"suxen_leader",
		map[string]string{"role": cleanupSchedulerMetricRole},
		1,
	)

	time.Sleep(leaseDuration + 100*time.Millisecond)
	expiredFamilies, err := fixture.Handler.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	assertMetricValue(
		t,
		metricFamiliesByName(expiredFamilies),
		"suxen_leader",
		map[string]string{"role": cleanupSchedulerMetricRole},
		0,
	)

	contender := New(
		config.Config{},
		fixture.Metadata,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	acquired, err = contender.acquireSchedulerLease(
		ctx,
		cleanupLeaseName,
		cleanupSchedulerMetricRole,
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("acquire cleanup lease after expiry: acquired=%v err=%v", acquired, err)
	}
	contenderFamilies, err := contender.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	contenderByName := metricFamiliesByName(contenderFamilies)
	assertMetricValue(
		t,
		contenderByName,
		"suxen_leader",
		map[string]string{"role": cleanupSchedulerMetricRole},
		1,
	)
	assertMetricValue(
		t,
		contenderByName,
		"suxen_leader_last_held_timestamp_seconds",
		map[string]string{"role": cleanupSchedulerMetricRole},
		-1,
	)

	if err := contender.waitForProvisionLease(ctx); err != nil {
		t.Fatal(err)
	}
	if err := contender.releaseProvisionLease(); err != nil {
		t.Fatal(err)
	}
	provisionFamilies, err := contender.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	provisionByName := metricFamiliesByName(provisionFamilies)
	assertMetricValue(
		t,
		provisionByName,
		"suxen_leader",
		map[string]string{"role": provisioningMetricRole},
		0,
	)
	assertMetricValue(
		t,
		provisionByName,
		"suxen_leader_last_held_timestamp_seconds",
		map[string]string{"role": provisioningMetricRole},
		-1,
	)
}

type controlledStatsStore struct {
	store.Store
	stats store.Stats
	err   error
	calls int
}

func (metadata *controlledStatsStore) Stats(context.Context) (store.Stats, error) {
	metadata.calls++
	return metadata.stats, metadata.err
}

func (metadata *controlledStatsStore) DatabasePoolStats() store.DatabasePoolStats {
	provider := metadata.Store.(store.DatabasePoolStatsProvider)
	return provider.DatabasePoolStats()
}

func parseMetricResponse(t *testing.T, response *http.Response) map[string]*dto.MetricFamily {
	t.Helper()
	defer response.Body.Close()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(response.Body)
	if err != nil {
		t.Fatalf("parse Prometheus metrics: %v", err)
	}
	return families
}

func metricFamiliesByName(families []*dto.MetricFamily) map[string]*dto.MetricFamily {
	result := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		result[family.GetName()] = family
	}
	return result
}

func assertMetricValue(
	t *testing.T,
	families map[string]*dto.MetricFamily,
	name string,
	labels map[string]string,
	want float64,
) {
	t.Helper()
	family := families[name]
	if family == nil {
		t.Fatalf("metric family %s was not gathered", name)
	}
	for _, metric := range family.Metric {
		if !metricHasLabels(metric, labels) {
			continue
		}
		var got float64
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			got = metric.GetCounter().GetValue()
		case dto.MetricType_GAUGE:
			got = metric.GetGauge().GetValue()
		default:
			t.Fatalf("metric %s has unsupported type %s", name, family.GetType())
		}
		if want >= 0 && got != want {
			t.Fatalf("metric %s labels %v = %v, want %v", name, labels, got, want)
		}
		return
	}
	t.Fatalf("metric %s has no series with labels %v", name, labels)
}

func assertHistogramCount(
	t *testing.T,
	families map[string]*dto.MetricFamily,
	name string,
	labels map[string]string,
	want uint64,
) {
	t.Helper()
	family := families[name]
	if family == nil {
		t.Fatalf("metric family %s was not gathered", name)
	}
	for _, metric := range family.Metric {
		if !metricHasLabels(metric, labels) {
			continue
		}
		if got := metric.GetHistogram().GetSampleCount(); got != want {
			t.Fatalf("histogram %s labels %v count = %d, want %d", name, labels, got, want)
		}
		return
	}
	t.Fatalf("histogram %s has no series with labels %v", name, labels)
}

func metricHasLabels(metric *dto.Metric, expected map[string]string) bool {
	if len(metric.Label) != len(expected) {
		return false
	}
	for _, label := range metric.Label {
		if expected[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}

func withLabel(labels map[string]string, name string, value string) map[string]string {
	result := make(map[string]string, len(labels)+1)
	for key, existing := range labels {
		result[key] = existing
	}
	result[name] = value
	return result
}
