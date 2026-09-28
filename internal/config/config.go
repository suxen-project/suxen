// Package config loads suxen process configuration from the environment.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suxen-project/suxen/internal/domain"
	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// Config contains process configuration loaded from SUXEN_* environment variables.
type Config struct {
	Listen      string
	DataDir     string
	DatabaseURL string
	BlobURL     string
	// BlobShared lets an operator assert that the configured blob store lives on
	// storage every replica can reach (e.g. a ReadWriteMany volume), so a
	// node-local driver such as fs:// can back a multi-replica deployment.
	BlobShared         bool
	BootstrapUser      string
	BootstrapPassword  string
	BootstrapToken     string
	Provision          string
	PublicURL          string
	OIDCStateSecret    string
	AuthFailureLimit   int
	AuthFailureWindow  time.Duration
	AuthLockout        time.Duration
	AuthTrustedProxies []netip.Prefix
	CleanupInterval    time.Duration
	GCInterval         time.Duration
	VerifyInterval     time.Duration
	MigrateInterval    time.Duration
	// Webhook delivery retry cadence. A failed delivery is retried after
	// WebhookRetryBase * 2^(attempt-1); the worker only re-claims due retries on
	// each WebhookPollInterval tick, so that interval is the floor between
	// attempts. A delivery is dead-lettered after WebhookMaxAttempts failures.
	WebhookRetryBase           time.Duration
	WebhookPollInterval        time.Duration
	WebhookMaxAttempts         int
	ProxyManifestTTL           time.Duration
	OutboundTimeout            time.Duration
	ProxyResponseHeaderTimeout time.Duration
	OutboundCIDRs              []netip.Prefix
	OutboundHosts              []string
	ProxyRealmHosts            []string
	ReadTimeout                time.Duration
	WriteTimeout               time.Duration
	MaxUploadBytes             int64
	MaxConcurrentUploads       int
	LogLevel                   string
	// Cluster enables database lease gating for singleton scheduled work.
	Cluster bool
	// DisableUI prevents UI routing in binaries that contain embedded UI assets.
	// The zero value keeps the UI enabled for programmatic Config construction.
	DisableUI bool
}

// Load reads and validates process configuration from the environment.
func Load() (Config, error) {
	dataDirectory := env("SUXEN_DATA", "./data")
	absoluteDataDirectory, err := filepath.Abs(dataDirectory)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}

	readTimeout, err := duration("SUXEN_READ_TIMEOUT", 0)
	if err != nil {
		return Config{}, err
	}
	if readTimeout < 0 {
		return Config{}, fmt.Errorf("SUXEN_READ_TIMEOUT cannot be negative")
	}
	writeTimeout, err := duration("SUXEN_WRITE_TIMEOUT", 0)
	if err != nil {
		return Config{}, err
	}
	if writeTimeout < 0 {
		return Config{}, fmt.Errorf("SUXEN_WRITE_TIMEOUT cannot be negative")
	}
	maxUploadBytes, err := integer("SUXEN_MAX_UPLOAD_BYTES", 10<<30)
	if err != nil {
		return Config{}, err
	}
	if maxUploadBytes <= 0 {
		return Config{}, fmt.Errorf("SUXEN_MAX_UPLOAD_BYTES must be positive")
	}
	maxConcurrentUploads, err := integer("SUXEN_MAX_CONCURRENT_UPLOADS", 4)
	if err != nil {
		return Config{}, err
	}
	if maxConcurrentUploads <= 0 || maxConcurrentUploads > 1024 {
		return Config{}, fmt.Errorf("SUXEN_MAX_CONCURRENT_UPLOADS must be between 1 and 1024")
	}
	authFailureLimit, err := integer("SUXEN_AUTH_FAILURE_LIMIT", 10)
	if err != nil {
		return Config{}, err
	}
	if authFailureLimit <= 0 {
		return Config{}, fmt.Errorf("SUXEN_AUTH_FAILURE_LIMIT must be positive")
	}
	authFailureWindow, err := duration("SUXEN_AUTH_FAILURE_WINDOW", time.Minute)
	if err != nil {
		return Config{}, err
	}
	if authFailureWindow <= 0 {
		return Config{}, fmt.Errorf("SUXEN_AUTH_FAILURE_WINDOW must be positive")
	}
	authLockout, err := duration("SUXEN_AUTH_LOCKOUT", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if authLockout <= 0 {
		return Config{}, fmt.Errorf("SUXEN_AUTH_LOCKOUT must be positive")
	}
	authTrustedProxies, err := cidrList("SUXEN_AUTH_TRUSTED_PROXY_CIDRS")
	if err != nil {
		return Config{}, err
	}
	cleanupInterval, err := duration("SUXEN_CLEANUP_INTERVAL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	if cleanupInterval < 0 {
		return Config{}, fmt.Errorf("SUXEN_CLEANUP_INTERVAL cannot be negative")
	}
	gcInterval, err := duration("SUXEN_GC_INTERVAL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	if gcInterval < 0 {
		return Config{}, fmt.Errorf("SUXEN_GC_INTERVAL cannot be negative")
	}
	// Verify re-reads blob stores and, when re-hashing, every referenced blob;
	// it stays opt-in (default 0 = disabled) so a routine deployment does no
	// scheduled integrity I/O until an operator asks for it.
	verifyInterval, err := duration("SUXEN_VERIFY_INTERVAL", 0)
	if err != nil {
		return Config{}, err
	}
	if verifyInterval < 0 {
		return Config{}, fmt.Errorf("SUXEN_VERIFY_INTERVAL cannot be negative")
	}
	// Migration advances draining blob stores; a short interval keeps a freshly
	// drained store moving without waiting. 0 disables the mover entirely.
	migrateInterval, err := duration("SUXEN_MIGRATE_INTERVAL", time.Minute)
	if err != nil {
		return Config{}, err
	}
	if migrateInterval < 0 {
		return Config{}, fmt.Errorf("SUXEN_MIGRATE_INTERVAL cannot be negative")
	}
	webhookRetryBase, err := duration("SUXEN_WEBHOOK_RETRY_BASE", time.Second)
	if err != nil {
		return Config{}, err
	}
	if webhookRetryBase <= 0 {
		return Config{}, fmt.Errorf("SUXEN_WEBHOOK_RETRY_BASE must be positive")
	}
	webhookPollInterval, err := duration("SUXEN_WEBHOOK_POLL_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	if webhookPollInterval <= 0 {
		return Config{}, fmt.Errorf("SUXEN_WEBHOOK_POLL_INTERVAL must be positive")
	}
	webhookMaxAttempts, err := integer("SUXEN_WEBHOOK_MAX_ATTEMPTS", 8)
	if err != nil {
		return Config{}, err
	}
	if webhookMaxAttempts <= 0 {
		return Config{}, fmt.Errorf("SUXEN_WEBHOOK_MAX_ATTEMPTS must be positive")
	}
	proxyManifestTTL, err := duration("SUXEN_PROXY_MANIFEST_TTL", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if proxyManifestTTL < 0 {
		return Config{}, fmt.Errorf("SUXEN_PROXY_MANIFEST_TTL cannot be negative")
	}
	outboundTimeout, err := duration("SUXEN_OUTBOUND_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	if outboundTimeout <= 0 {
		return Config{}, fmt.Errorf("SUXEN_OUTBOUND_TIMEOUT must be positive")
	}
	proxyResponseHeaderTimeout, err := duration("SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	if proxyResponseHeaderTimeout <= 0 {
		return Config{}, fmt.Errorf("SUXEN_PROXY_RESPONSE_HEADER_TIMEOUT must be positive")
	}
	outboundCIDRs, err := cidrList("SUXEN_OUTBOUND_ALLOWED_CIDRS")
	if err != nil {
		return Config{}, err
	}
	outboundHosts, err := hostList("SUXEN_OUTBOUND_ALLOWED_HOSTS")
	if err != nil {
		return Config{}, err
	}
	proxyRealmHosts, err := hostList("SUXEN_PROXY_TOKEN_REALM_HOSTS")
	if err != nil {
		return Config{}, err
	}
	uiEnabled, err := boolean("SUXEN_UI_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	clusterValue := strings.TrimSpace(os.Getenv("SUXEN_CLUSTER"))
	clusterConfigured := clusterValue != ""
	cluster := false
	if clusterConfigured {
		cluster, err = strconv.ParseBool(clusterValue)
		if err != nil {
			return Config{}, fmt.Errorf("parse SUXEN_CLUSTER: %w", err)
		}
	}
	blobShared, err := boolean("SUXEN_BLOBSTORE_SHARED", false)
	if err != nil {
		return Config{}, err
	}
	publicURL, err := publicOrigin(os.Getenv("SUXEN_PUBLIC_URL"))
	if err != nil {
		return Config{}, err
	}

	config := Config{
		Listen:                     env("SUXEN_LISTEN", "127.0.0.1:8080"),
		DataDir:                    absoluteDataDirectory,
		DatabaseURL:                env("SUXEN_DB", defaultDatabaseURL(absoluteDataDirectory)),
		BlobURL:                    env("SUXEN_BLOBSTORE", defaultBlobURL(absoluteDataDirectory)),
		BlobShared:                 blobShared,
		BootstrapUser:              env("SUXEN_BOOTSTRAP_USER", "admin"),
		BootstrapPassword:          os.Getenv("SUXEN_BOOTSTRAP_PASSWORD"),
		BootstrapToken:             os.Getenv("SUXEN_BOOTSTRAP_TOKEN"),
		Provision:                  os.Getenv("SUXEN_PROVISION"),
		PublicURL:                  publicURL,
		OIDCStateSecret:            os.Getenv("SUXEN_OIDC_STATE_SECRET"),
		AuthFailureLimit:           int(authFailureLimit),
		AuthFailureWindow:          authFailureWindow,
		AuthLockout:                authLockout,
		AuthTrustedProxies:         authTrustedProxies,
		CleanupInterval:            cleanupInterval,
		GCInterval:                 gcInterval,
		VerifyInterval:             verifyInterval,
		MigrateInterval:            migrateInterval,
		WebhookRetryBase:           webhookRetryBase,
		WebhookPollInterval:        webhookPollInterval,
		WebhookMaxAttempts:         int(webhookMaxAttempts),
		ProxyManifestTTL:           proxyManifestTTL,
		OutboundTimeout:            outboundTimeout,
		ProxyResponseHeaderTimeout: proxyResponseHeaderTimeout,
		OutboundCIDRs:              outboundCIDRs,
		OutboundHosts:              outboundHosts,
		ProxyRealmHosts:            proxyRealmHosts,
		ReadTimeout:                readTimeout,
		WriteTimeout:               writeTimeout,
		MaxUploadBytes:             maxUploadBytes,
		MaxConcurrentUploads:       int(maxConcurrentUploads),
		LogLevel:                   env("SUXEN_LOG_LEVEL", "info"),
		Cluster:                    cluster,
		DisableUI:                  !uiEnabled,
	}
	if !domain.ValidUsername(config.BootstrapUser) {
		return Config{}, fmt.Errorf("SUXEN_BOOTSTRAP_USER: %w", domain.ErrInvalidUsername)
	}

	if !hasAnyPrefix(config.DatabaseURL, "sqlite://", "postgres://", "postgresql://") {
		return Config{}, fmt.Errorf("SUXEN_DB must use sqlite://, postgres://, or postgresql://")
	}
	// The default blob store is selected by URL scheme against the compiled-in
	// driver registry, so a build without a driver rejects its scheme here.
	blobDriver, blobDriverFound := spiblob.ForURL(config.BlobURL)
	if !blobDriverFound {
		return Config{}, fmt.Errorf(
			"SUXEN_BLOBSTORE must use a compiled blob store URL scheme (available: %s)",
			availableBlobSchemes(),
		)
	}
	postgresConfigured := hasAnyPrefix(config.DatabaseURL, "postgres://", "postgresql://")
	// Cluster safety is a driver capability: only drivers on shared storage can
	// serve multiple replicas. SUXEN_BLOBSTORE_SHARED lets an operator vouch for
	// a node-local driver (fs://) that actually sits on a ReadWriteMany volume.
	sharedBlob := blobDriver.SharedStorage || config.BlobShared
	if clusterConfigured && !config.Cluster && (postgresConfigured || sharedBlob) {
		return Config{}, fmt.Errorf(
			"SUXEN_CLUSTER=false is unsafe with Postgres or shared blob storage; omit it for automatic lease gating or set it to true with both backends",
		)
	}
	if clusterConfigured && config.Cluster && (!postgresConfigured || !sharedBlob) {
		return Config{}, fmt.Errorf(
			"SUXEN_CLUSTER=true requires Postgres and a shared blob store driver",
		)
	}
	if !clusterConfigured && (postgresConfigured || sharedBlob) {
		config.Cluster = true
	}
	if config.OIDCStateSecret != "" && len(config.OIDCStateSecret) < 32 {
		return Config{}, fmt.Errorf("SUXEN_OIDC_STATE_SECRET must contain at least 32 characters")
	}
	if config.Cluster || postgresConfigured || sharedBlob {
		if err := validateBootstrapCredentials(config); err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func availableBlobSchemes() string {
	schemes := spiblob.URLSchemes()
	for index, scheme := range schemes {
		schemes[index] = scheme + "://"
	}
	if len(schemes) == 0 {
		return "none compiled in"
	}
	return strings.Join(schemes, ", ")
}

func publicOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse SUXEN_PUBLIC_URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("SUXEN_PUBLIC_URL must use http or https")
	}
	if parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("SUXEN_PUBLIC_URL must be an absolute origin")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("SUXEN_PUBLIC_URL must not contain user information")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("SUXEN_PUBLIC_URL must not contain a path")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("SUXEN_PUBLIC_URL must not contain a query or fragment")
	}
	return scheme + "://" + parsed.Host, nil
}

func validateBootstrapCredentials(config Config) error {
	if utf8.RuneCountInString(config.BootstrapPassword) < 12 || weakBootstrapCredential(config.BootstrapPassword) {
		return fmt.Errorf(
			"SUXEN_BOOTSTRAP_PASSWORD must be explicitly set to at least 12 non-placeholder characters for Postgres, shared blob storage, or cluster mode",
		)
	}
	if utf8.RuneCountInString(config.BootstrapToken) < 24 || weakBootstrapCredential(config.BootstrapToken) {
		return fmt.Errorf(
			"SUXEN_BOOTSTRAP_TOKEN must be explicitly set to at least 24 non-placeholder characters for Postgres, shared blob storage, or cluster mode",
		)
	}
	return nil
}

func weakBootstrapCredential(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "change-me", "change-me-token", "changeme", "password", "admin":
		return true
	default:
		return false
	}
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func defaultDatabaseURL(dataDirectory string) string {
	return "sqlite://" + filepath.Join(dataDirectory, "suxen.db")
}

func defaultBlobURL(dataDirectory string) string {
	return "fs://" + filepath.Join(dataDirectory, "blobs")
}

func env(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func duration(key string, defaultValue time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func integer(key string, defaultValue int64) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func boolean(key string, defaultValue bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func cidrList(key string) ([]netip.Prefix, error) {
	values := commaSeparated(os.Getenv(key))
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("parse %s entry %q: %w", key, value, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func hostList(key string) ([]string, error) {
	values := commaSeparated(os.Getenv(key))
	for index, value := range values {
		host := strings.ToLower(strings.TrimSuffix(value, "."))
		if host == "" || strings.ContainsAny(host, "/@ 	\r\n") {
			return nil, fmt.Errorf("%s entry %q must be an exact host without a scheme or path", key, value)
		}
		if strings.Contains(host, "*") {
			return nil, fmt.Errorf("%s entry %q must not contain a wildcard", key, value)
		}
		if strings.Contains(host, ":") {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, fmt.Errorf("%s entry %q must not contain a port", key, value)
			}
		}
		values[index] = host
	}
	return values, nil
}

func commaSeparated(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}
