package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsInvalidNumericConfiguration(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_MAX_UPLOAD_BYTES", "many")
	if _, err := Load(); err == nil {
		t.Fatal("invalid upload size was accepted")
	}
}

func TestLoadRejectsUnaddressableBootstrapUsername(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_BOOTSTRAP_USER", "ops/team")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUXEN_BOOTSTRAP_USER") {
		t.Fatalf("Load error = %v, want bootstrap username validation", err)
	}
}

func TestLoadBuildsSingleNodeDefaults(t *testing.T) {
	clearOptionalEnvironment(t)
	dataDirectory := t.TempDir()
	t.Setenv("SUXEN_DATA", dataDirectory)
	t.Setenv("SUXEN_MAX_UPLOAD_BYTES", "1024")

	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseURL != "sqlite://"+dataDirectory+"/suxen.db" {
		t.Fatalf("unexpected database URL %q", config.DatabaseURL)
	}
	if config.BlobURL != "fs://"+dataDirectory+"/blobs" {
		t.Fatalf("unexpected blob URL %q", config.BlobURL)
	}
	if config.Listen != "127.0.0.1:8080" {
		t.Fatalf("unexpected listen address %q", config.Listen)
	}
	if config.MaxUploadBytes != 1024 {
		t.Fatalf("got max upload size %d, want 1024", config.MaxUploadBytes)
	}
	if config.MaxConcurrentUploads != 4 {
		t.Fatalf("max concurrent uploads = %d, want 4", config.MaxConcurrentUploads)
	}
	if config.AuthFailureLimit != 10 {
		t.Fatalf("authentication failure limit = %d, want 10", config.AuthFailureLimit)
	}
	if config.AuthFailureWindow != time.Minute {
		t.Fatalf("authentication failure window = %s", config.AuthFailureWindow)
	}
	if config.AuthLockout != 5*time.Minute {
		t.Fatalf("authentication lockout = %s", config.AuthLockout)
	}
	if config.CleanupInterval.String() != "1h0m0s" {
		t.Fatalf("unexpected cleanup interval %s", config.CleanupInterval)
	}
	if config.GCInterval != time.Hour {
		t.Fatalf("unexpected garbage-collection interval %s", config.GCInterval)
	}
	if config.WebhookRetryBase != time.Second {
		t.Fatalf("webhook retry base = %s, want 1s", config.WebhookRetryBase)
	}
	if config.WebhookPollInterval != time.Second {
		t.Fatalf("webhook poll interval = %s, want 1s", config.WebhookPollInterval)
	}
	if config.WebhookMaxAttempts != 8 {
		t.Fatalf("webhook max attempts = %d, want 8", config.WebhookMaxAttempts)
	}
	if config.ProxyManifestTTL != 5*time.Minute {
		t.Fatalf("unexpected proxy manifest TTL %s", config.ProxyManifestTTL)
	}
	if config.DisableUI {
		t.Fatal("administration UI is disabled by default")
	}
	if config.Cluster {
		t.Fatal("single-node defaults unexpectedly enabled cluster lease gating")
	}
}

func TestLoadRejectsInvalidAuthenticationThrottle(t *testing.T) {
	for _, test := range []struct {
		key   string
		value string
	}{
		{key: "SUXEN_AUTH_FAILURE_LIMIT", value: "0"},
		{key: "SUXEN_AUTH_FAILURE_LIMIT", value: "many"},
		{key: "SUXEN_AUTH_FAILURE_WINDOW", value: "0s"},
		{key: "SUXEN_AUTH_LOCKOUT", value: "-1s"},
		{key: "SUXEN_AUTH_TRUSTED_PROXY_CIDRS", value: "10.20.0.0"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("invalid throttle error = %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidUploadConcurrency(t *testing.T) {
	for _, value := range []string{"0", "1025", "many"} {
		t.Run(value, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv("SUXEN_MAX_CONCURRENT_UPLOADS", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUXEN_MAX_CONCURRENT_UPLOADS") {
				t.Fatalf("invalid upload concurrency error = %v", err)
			}
		})
	}
}

func TestLoadRejectsNegativeSchedulerIntervals(t *testing.T) {
	for _, variable := range []string{"SUXEN_CLEANUP_INTERVAL", "SUXEN_GC_INTERVAL"} {
		t.Run(variable, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(variable, "-1s")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("negative %s error = %v", variable, err)
			}
		})
	}
}

func TestLoadWebhookRetryCadence(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_WEBHOOK_RETRY_BASE", "50ms")
	t.Setenv("SUXEN_WEBHOOK_POLL_INTERVAL", "100ms")
	t.Setenv("SUXEN_WEBHOOK_MAX_ATTEMPTS", "4")

	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.WebhookRetryBase != 50*time.Millisecond {
		t.Fatalf("webhook retry base = %s, want 50ms", config.WebhookRetryBase)
	}
	if config.WebhookPollInterval != 100*time.Millisecond {
		t.Fatalf("webhook poll interval = %s, want 100ms", config.WebhookPollInterval)
	}
	if config.WebhookMaxAttempts != 4 {
		t.Fatalf("webhook max attempts = %d, want 4", config.WebhookMaxAttempts)
	}
}

func TestLoadRejectsNonPositiveWebhookCadence(t *testing.T) {
	for _, test := range []struct {
		key   string
		value string
	}{
		{key: "SUXEN_WEBHOOK_RETRY_BASE", value: "0s"},
		{key: "SUXEN_WEBHOOK_POLL_INTERVAL", value: "-1s"},
		{key: "SUXEN_WEBHOOK_MAX_ATTEMPTS", value: "0"},
		{key: "SUXEN_WEBHOOK_MAX_ATTEMPTS", value: "many"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("invalid webhook cadence error = %v", err)
			}
		})
	}
}

func TestLoadAcceptsDisabledAdministrationUI(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_UI_ENABLED", "false")

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.DisableUI {
		t.Fatal("SUXEN_UI_ENABLED=false did not disable the administration UI")
	}
}

func TestLoadRejectsInvalidAdministrationUISetting(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_UI_ENABLED", "sometimes")

	if _, err := Load(); err == nil {
		t.Fatal("invalid SUXEN_UI_ENABLED value was accepted")
	}
}

func TestLoadAcceptsDisabledProxyManifestCache(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_PROXY_MANIFEST_TTL", "0")

	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.ProxyManifestTTL != 0 {
		t.Fatalf("got proxy manifest TTL %s, want zero", config.ProxyManifestTTL)
	}
}

func TestLoadRejectsNegativeProxyManifestTTL(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_PROXY_MANIFEST_TTL", "-1s")

	if _, err := Load(); err == nil {
		t.Fatal("negative proxy manifest TTL was accepted")
	}
}

func TestLoadRejectsShortOIDCStateSecret(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_OIDC_STATE_SECRET", "too-short")
	if _, err := Load(); err == nil {
		t.Fatal("short OIDC state secret was accepted")
	}
}

func TestLoadAcceptsPostgresMetadataURL(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen?sslmode=disable")
	setStrongBootstrapCredentials(t)
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseURL != "postgres://suxen:secret@database/suxen?sslmode=disable" {
		t.Fatalf("unexpected PostgreSQL URL %q", config.DatabaseURL)
	}
	if !config.Cluster {
		t.Fatal("Postgres did not enable cleanup lease gating")
	}
}

func TestLoadAcceptsS3BlobURL(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_BLOBSTORE", "s3://artifact-bucket/suxen?region=eu-west-1")
	setStrongBootstrapCredentials(t)
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.BlobURL != "s3://artifact-bucket/suxen?region=eu-west-1" {
		t.Fatalf("unexpected S3 blob URL %q", config.BlobURL)
	}
	if !config.Cluster {
		t.Fatal("S3 did not enable cleanup lease gating")
	}
}

func TestLoadRequiresStrongBootstrapCredentialsForDistributedModes(t *testing.T) {
	tests := map[string]struct {
		key   string
		value string
	}{
		"cluster":  {"SUXEN_CLUSTER", "true"},
		"postgres": {"SUXEN_DB", "postgres://suxen:secret@database/suxen"},
		"s3":       {"SUXEN_BLOBSTORE", "s3://artifact-bucket/suxen"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(test.key, test.value)
			if name == "cluster" {
				t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen")
				t.Setenv("SUXEN_BLOBSTORE", "s3://artifact-bucket/suxen")
			}

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUXEN_BOOTSTRAP_PASSWORD") {
				t.Fatalf("missing bootstrap password error = %v", err)
			}

			t.Setenv("SUXEN_BOOTSTRAP_PASSWORD", "change-me")
			t.Setenv("SUXEN_BOOTSTRAP_TOKEN", "change-me-token")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUXEN_BOOTSTRAP_PASSWORD") {
				t.Fatalf("placeholder bootstrap password error = %v", err)
			}

			t.Setenv("SUXEN_BOOTSTRAP_PASSWORD", "a-strong-bootstrap-password")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUXEN_BOOTSTRAP_TOKEN") {
				t.Fatalf("placeholder bootstrap token error = %v", err)
			}

			t.Setenv("SUXEN_BOOTSTRAP_TOKEN", "a-strong-bootstrap-token-value")
			loaded, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !loaded.Cluster {
				t.Fatalf("%s mode did not enable cleanup lease gating", name)
			}
		})
	}
}

func TestLoadRejectsUnsafeClusterSelections(t *testing.T) {
	t.Run("distributed backend with explicit false", func(t *testing.T) {
		clearOptionalEnvironment(t)
		t.Setenv("SUXEN_DATA", t.TempDir())
		t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen")
		t.Setenv("SUXEN_CLUSTER", "false")
		setStrongBootstrapCredentials(t)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("explicitly disabled distributed lease error = %v", err)
		}
	})

	t.Run("cluster without shared backends", func(t *testing.T) {
		clearOptionalEnvironment(t)
		t.Setenv("SUXEN_DATA", t.TempDir())
		t.Setenv("SUXEN_CLUSTER", "true")
		setStrongBootstrapCredentials(t)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires Postgres and a shared blob store driver") {
			t.Fatalf("incomplete cluster topology error = %v", err)
		}
	})

	t.Run("cluster on a node-local filesystem without the shared assertion", func(t *testing.T) {
		clearOptionalEnvironment(t)
		t.Setenv("SUXEN_DATA", t.TempDir())
		t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen")
		t.Setenv("SUXEN_CLUSTER", "true")
		setStrongBootstrapCredentials(t)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires Postgres and a shared blob store driver") {
			t.Fatalf("fs without SUXEN_BLOBSTORE_SHARED error = %v", err)
		}
	})
}

func TestLoadAcceptsSharedFilesystemBlobStoreForCluster(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen")
	t.Setenv("SUXEN_CLUSTER", "true")
	t.Setenv("SUXEN_BLOBSTORE_SHARED", "true")
	setStrongBootstrapCredentials(t)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("shared filesystem cluster config rejected: %v", err)
	}
	if !loaded.BlobShared {
		t.Fatalf("BlobShared = false, want true")
	}
	if !loaded.Cluster {
		t.Fatalf("Cluster = false, want true")
	}
}

func TestLoadTreatsSharedFilesystemAsDistributedForAutoLeaseGating(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_DB", "postgres://suxen:secret@database/suxen")
	t.Setenv("SUXEN_BLOBSTORE_SHARED", "true")
	setStrongBootstrapCredentials(t)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("shared filesystem without explicit cluster rejected: %v", err)
	}
	if !loaded.Cluster {
		t.Fatalf("Cluster = false; a shared blob store should auto-enable lease gating")
	}
}

func TestLoadValidatesPublicURLAsAnOrigin(t *testing.T) {
	valid := map[string]string{
		"HTTPS origin":   "https://packages.example",
		"trailing slash": "https://packages.example/",
		"development":    "http://localhost:8080",
		"IPv6":           "https://[2001:db8::1]:8443",
	}
	for name, value := range valid {
		t.Run(name, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv("SUXEN_PUBLIC_URL", value)
			loaded, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(loaded.PublicURL, "/") {
				t.Fatalf("public URL was not normalized: %q", loaded.PublicURL)
			}
		})
	}

	invalid := []string{
		"packages.example",
		"ftp://packages.example",
		"https://",
		"https://user:secret@packages.example",
		"https://packages.example/suxen",
		"https://packages.example?tenant=one",
		"https://packages.example#callbacks",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv("SUXEN_PUBLIC_URL", value)
			if _, err := Load(); err == nil {
				t.Fatalf("invalid public URL %q was accepted", value)
			}
		})
	}
}

func TestLoadRejectsInvalidClusterSetting(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_CLUSTER", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("invalid SUXEN_CLUSTER value was accepted")
	}
}

func TestLoadParsesOutboundPolicy(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	t.Setenv("SUXEN_OUTBOUND_TIMEOUT", "12s")
	t.Setenv("SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT", "45s")
	t.Setenv("SUXEN_OUTBOUND_ALLOWED_CIDRS", "10.20.0.0/16, fd00:1234::/48")
	t.Setenv("SUXEN_OUTBOUND_ALLOWED_HOSTS", "Registry.Internal., identity.internal")
	t.Setenv("SUXEN_PROXY_TOKEN_REALM_HOSTS", "tokens.example.com")

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OutboundTimeout != 12*time.Second {
		t.Fatalf("unexpected outbound timeout %s", loaded.OutboundTimeout)
	}
	if loaded.ProxyResponseHeaderTimeout != 45*time.Second {
		t.Fatalf("unexpected proxy response-header timeout %s", loaded.ProxyResponseHeaderTimeout)
	}
	wantCIDRs := []netip.Prefix{
		netip.MustParsePrefix("10.20.0.0/16"),
		netip.MustParsePrefix("fd00:1234::/48"),
	}
	if !reflect.DeepEqual(loaded.OutboundCIDRs, wantCIDRs) {
		t.Fatalf("got outbound CIDRs %v, want %v", loaded.OutboundCIDRs, wantCIDRs)
	}
	wantHosts := []string{"registry.internal", "identity.internal"}
	if !reflect.DeepEqual(loaded.OutboundHosts, wantHosts) {
		t.Fatalf("got outbound hosts %v, want %v", loaded.OutboundHosts, wantHosts)
	}
}

func TestLoadLeavesStreamingTransfersUnboundedByDefault(t *testing.T) {
	clearOptionalEnvironment(t)
	t.Setenv("SUXEN_DATA", t.TempDir())
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReadTimeout != 0 || loaded.WriteTimeout != 0 {
		t.Fatalf("streaming deadlines = read %s, write %s; want zero", loaded.ReadTimeout, loaded.WriteTimeout)
	}
	if loaded.OutboundTimeout != 30*time.Second {
		t.Fatalf("control-plane timeout = %s, want 30s", loaded.OutboundTimeout)
	}
	if loaded.ProxyResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("proxy response-header timeout = %s, want 30s", loaded.ProxyResponseHeaderTimeout)
	}
}

func TestLoadRejectsNegativeStreamingTimeouts(t *testing.T) {
	for _, variable := range []string{"SUXEN_READ_TIMEOUT", "SUXEN_WRITE_TIMEOUT"} {
		t.Run(variable, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(variable, "-1s")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("negative timeout error = %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidOutboundPolicy(t *testing.T) {
	tests := map[string]struct {
		key   string
		value string
	}{
		"non-positive timeout":              {"SUXEN_OUTBOUND_TIMEOUT", "0s"},
		"non-positive proxy header timeout": {"SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT", "0s"},
		"invalid CIDR":                      {"SUXEN_OUTBOUND_ALLOWED_CIDRS", "10.20.0.0"},
		"host wildcard":                     {"SUXEN_OUTBOUND_ALLOWED_HOSTS", "*.internal"},
		"host URL":                          {"SUXEN_OUTBOUND_ALLOWED_HOSTS", "https://identity.internal"},
		"realm host port":                   {"SUXEN_PROXY_TOKEN_REALM_HOSTS", "tokens.example.com:8443"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			clearOptionalEnvironment(t)
			t.Setenv("SUXEN_DATA", t.TempDir())
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil {
				t.Fatalf("invalid %s value %q was accepted", test.key, test.value)
			}
		})
	}
}

func clearOptionalEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SUXEN_DB",
		"SUXEN_BLOBSTORE",
		"SUXEN_MAX_UPLOAD_BYTES",
		"SUXEN_MAX_CONCURRENT_UPLOADS",
		"SUXEN_BOOTSTRAP_USER",
		"SUXEN_BOOTSTRAP_PASSWORD",
		"SUXEN_BOOTSTRAP_TOKEN",
		"SUXEN_READ_TIMEOUT",
		"SUXEN_WRITE_TIMEOUT",
		"SUXEN_PUBLIC_URL",
		"SUXEN_OIDC_STATE_SECRET",
		"SUXEN_AUTH_FAILURE_LIMIT",
		"SUXEN_AUTH_FAILURE_WINDOW",
		"SUXEN_AUTH_LOCKOUT",
		"SUXEN_AUTH_TRUSTED_PROXY_CIDRS",
		"SUXEN_CLEANUP_INTERVAL",
		"SUXEN_GC_INTERVAL",
		"SUXEN_WEBHOOK_RETRY_BASE",
		"SUXEN_WEBHOOK_POLL_INTERVAL",
		"SUXEN_WEBHOOK_MAX_ATTEMPTS",
		"SUXEN_PROXY_MANIFEST_TTL",
		"SUXEN_OUTBOUND_TIMEOUT",
		"SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT",
		"SUXEN_OUTBOUND_ALLOWED_CIDRS",
		"SUXEN_OUTBOUND_ALLOWED_HOSTS",
		"SUXEN_PROXY_TOKEN_REALM_HOSTS",
		"SUXEN_UI_ENABLED",
		"SUXEN_CLUSTER",
		"SUXEN_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

func setStrongBootstrapCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("SUXEN_BOOTSTRAP_PASSWORD", "a-strong-bootstrap-password")
	t.Setenv("SUXEN_BOOTSTRAP_TOKEN", "a-strong-bootstrap-token-value")
}
