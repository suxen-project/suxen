package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestInternalRepositoryIDIsNeverSerialized(t *testing.T) {
	repository := Repository{ID: "internal-repo-id", Name: "public", Format: "raw", Type: "hosted"}
	encoded, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "internal-repo-id") {
		t.Fatalf("repository JSON leaked the internal id: %s", encoded)
	}

	asset := Asset{Repository: "public", RepositoryID: "internal-repo-id", Path: "x"}
	encodedAsset, err := json.Marshal(asset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedAsset), "internal-repo-id") {
		t.Fatalf("asset JSON leaked the internal repository id: %s", encodedAsset)
	}
}

func TestResourceNameAndUsernameByteLimits(t *testing.T) {
	if !validName(strings.Repeat("a", MaxResourceNameBytes)) ||
		validName(strings.Repeat("a", MaxResourceNameBytes+1)) {
		t.Fatal("resource name byte limit does not accept its boundary")
	}
	if !ValidUsername(strings.Repeat("a", MaxUsernameBytes)) ||
		ValidUsername(strings.Repeat("a", MaxUsernameBytes+1)) {
		t.Fatal("username byte limit does not accept its boundary")
	}
}

func TestValidAssetPathRequiresValidUTF8WithinByteLimit(t *testing.T) {
	valid := strings.Repeat("é", MaxAssetPathBytes/2)
	if !ValidAssetPath(valid) {
		t.Fatalf("valid %d-byte UTF-8 path rejected", len(valid))
	}
	for _, invalid := range []string{"", valid + "x", "bad\xff", "bad\xc3"} {
		if ValidAssetPath(invalid) {
			t.Fatalf("invalid path %q accepted", invalid)
		}
	}
}

func TestSameUpstreamEndpoint(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"identical", "https://o.test/raw", "https://o.test/raw", true},
		{"both empty", "", "", true},
		{"credentials rotated", "https://o.test/raw", "https://u:p@o.test/raw", true},
		{"credentials changed", "https://a:b@o.test/raw", "https://c:d@o.test/raw", true},
		{"path changed", "https://o.test/raw", "https://o.test/oci", false},
		{"host changed", "https://o.test/raw", "https://other.test/raw", false},
		{"scheme changed", "http://o.test/raw", "https://o.test/raw", false},
		{"query changed", "https://o.test/raw?a=1", "https://o.test/raw?a=2", false},
		{"one empty", "https://o.test/raw", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SameUpstreamEndpoint(tc.a, tc.b)
			if err != nil {
				t.Fatalf("SameUpstreamEndpoint(%q,%q) error: %v", tc.a, tc.b, err)
			}
			if got != tc.want {
				t.Fatalf("SameUpstreamEndpoint(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestRepositoryJSONRedactsUpstreamCredentials(t *testing.T) {
	repository := Repository{
		Name:     "private-proxy",
		Format:   "oci",
		Type:     "proxy",
		Upstream: "https://reader:secret@registry.example/base?token=query-secret#fragment-secret",
	}
	encoded, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "reader") || strings.Contains(string(encoded), "secret") ||
		strings.Contains(string(encoded), "token") {
		t.Fatalf("repository JSON leaked upstream credentials: %s", encoded)
	}
	if !strings.Contains(string(encoded), "https://registry.example/base") {
		t.Fatalf("repository JSON lost sanitized upstream URL: %s", encoded)
	}
}

func TestBlobStoreUploadSessionAttributesValidation(t *testing.T) {
	valid := BlobStore{
		Name:             "constrained",
		Driver:           "fs",
		ConfigurationRef: &ConfigurationReference{Env: "CONSTRAINED_STORE"},
		PhysicalIdentity: strings.Repeat("a", 64),
		Attributes: map[string]any{
			"location": "local-ssd",
			"uploadSessions": map[string]any{
				"staleAfter":              "15m",
				"maxStagedBytes":          float64(5 << 30),
				"maxPrincipalStagedBytes": int64(1 << 30),
				"maxPrincipalSessions":    2,
			},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid upload-session policy returned %v", err)
	}
	for _, literal := range []json.Number{"1e1", "10.0", "9007199254740993", "9223372036854775807"} {
		candidate := valid
		candidate.Attributes = map[string]any{"uploadSessions": map[string]any{"maxPrincipalSessions": literal}}
		if err := candidate.Validate(); err != nil {
			t.Errorf("valid JSON limit %q rejected: %v", literal, err)
		}
	}

	tests := []struct {
		name   string
		policy any
	}{
		{name: "not an object", policy: "15m"},
		{name: "unknown field", policy: map[string]any{"eager": true}},
		{name: "invalid duration", policy: map[string]any{"staleAfter": "soon"}},
		{name: "zero duration", policy: map[string]any{"staleAfter": "0s"}},
		{name: "fractional sessions", policy: map[string]any{"maxPrincipalSessions": 1.5}},
		{name: "zero bytes", policy: map[string]any{"maxStagedBytes": 0}},
		{name: "fractional JSON", policy: map[string]any{"maxPrincipalSessions": json.Number("1.5")}},
		{name: "overflow JSON", policy: map[string]any{"maxPrincipalSessions": json.Number("9223372036854775808")}},
		{name: "huge exponent", policy: map[string]any{"maxPrincipalSessions": json.Number("1e1000000")}},
		{name: "invalid JSON number", policy: map[string]any{"maxPrincipalSessions": json.Number("1e+")}},
		{name: "overflow unsigned", policy: map[string]any{"maxPrincipalSessions": uint64(math.MaxInt64) + 1}},
		{name: "overflow float", policy: map[string]any{"maxPrincipalSessions": float64(0x1p63)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Attributes = map[string]any{"uploadSessions": test.policy}
			if err := candidate.Validate(); !errors.Is(err, ErrInvalidBlobStoreAttributes) {
				t.Fatalf("Validate() returned %v, want ErrInvalidBlobStoreAttributes", err)
			}
		})
	}
}

func TestPositiveInt64NativeNumbers(t *testing.T) {
	for _, test := range []struct {
		value any
		want  int64
		ok    bool
	}{
		{int(1), 1, true},
		{int8(2), 2, true},
		{int16(3), 3, true},
		{int32(4), 4, true},
		{int64(5), 5, true},
		{uint(6), 6, true},
		{uint8(7), 7, true},
		{uint16(8), 8, true},
		{uint32(9), 9, true},
		{uint64(math.MaxInt64), math.MaxInt64, true},
		{float32(10), 10, true},
		{float64(11), 11, true},
		{float64(11.5), 0, false},
		{float64(0x1p63), 0, false},
		{math.NaN(), 0, false},
		{uint64(math.MaxInt64) + 1, 0, false},
	} {
		got, ok := PositiveInt64(test.value)
		if ok != test.ok || ok && got != test.want {
			t.Errorf("PositiveInt64(%v) = (%d, %t), want (%d, %t)", test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestOCIRepositoryEndpointsValidation(t *testing.T) {
	valid := Repository{
		Name:   "docker",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &RepositoryEndpoints{
			Hosts: []string{"Registry.Example.com"},
			Ports: []int{5000},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid OCI endpoints: %v", err)
	}

	raw := valid
	raw.Format = "raw"
	if err := raw.Validate(); !errors.Is(err, ErrOCIEndpointsNotSupported) {
		t.Fatalf("raw endpoints = %v, want ErrOCIEndpointsNotSupported", err)
	}

	for _, host := range []string{"", "https://registry.example.com", "registry.example.com:5000", "registry.example.com/v2"} {
		invalid := valid
		invalid.Endpoints = &RepositoryEndpoints{Hosts: []string{host}}
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidOCIEndpoint) {
			t.Fatalf("host %q Validate() = %v, want ErrInvalidOCIEndpoint", host, err)
		}
	}

	duplicateHost := valid
	duplicateHost.Endpoints = &RepositoryEndpoints{Hosts: []string{"a.example.com", "A.example.com"}}
	if err := duplicateHost.Validate(); !errors.Is(err, ErrInvalidOCIEndpoint) {
		t.Fatalf("duplicate hosts Validate() = %v, want ErrInvalidOCIEndpoint", err)
	}

	duplicatePort := valid
	duplicatePort.Endpoints = &RepositoryEndpoints{Ports: []int{5000, 5000}}
	if err := duplicatePort.Validate(); !errors.Is(err, ErrInvalidOCIEndpoint) {
		t.Fatalf("duplicate ports Validate() = %v, want ErrInvalidOCIEndpoint", err)
	}
}

func TestCanonicalOCIRequestHostStripsPort(t *testing.T) {
	if got := CanonicalOCIRequestHost("Registry.Example.com:5000"); got != "registry.example.com" {
		t.Fatalf("CanonicalOCIRequestHost() = %q", got)
	}
}

func TestProxyRepositoryRequiresHTTPUpstream(t *testing.T) {
	valid := Repository{
		Name:     "central",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://reader:secret@packages.example.test/base",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid proxy repository: %v", err)
	}

	for _, upstream := range []string{
		"registry.example.test",
		"http:registry.example.test",
		"file:///tmp/packages",
	} {
		t.Run(upstream, func(t *testing.T) {
			repository := valid
			repository.Upstream = upstream
			if err := repository.Validate(); !errors.Is(err, ErrInvalidUpstream) {
				t.Fatalf("Validate() = %v, want ErrInvalidUpstream", err)
			}
		})
	}
}

func TestOIDCProviderRequiresAbsoluteIssuerURL(t *testing.T) {
	valid := OIDCProvider{
		Name:     "corporate",
		Issuer:   "https://identity.example.test/realms/workforce",
		ClientID: "suxen",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid OIDC provider: %v", err)
	}

	for _, issuer := range []string{
		"http:identity.example.test",
		"https://user@identity.example.test",
		"https://identity.example.test?tenant=workforce",
		"https://identity.example.test#issuer",
	} {
		t.Run(issuer, func(t *testing.T) {
			provider := valid
			provider.Issuer = issuer
			if err := provider.Validate(); !errors.Is(err, ErrInvalidOIDCIssuer) {
				t.Fatalf("Validate() = %v, want ErrInvalidOIDCIssuer", err)
			}
		})
	}
}

func TestParseRetentionDuration(t *testing.T) {
	for value, expected := range map[string]time.Duration{
		"36h":  36 * time.Hour,
		"2d":   48 * time.Hour,
		"1.5w": 252 * time.Hour,
		"6mo":  180 * 24 * time.Hour,
		"1y":   365 * 24 * time.Hour,
	} {
		actual, err := ParseRetentionDuration(value)
		if err != nil {
			t.Fatalf("ParseRetentionDuration(%q): %v", value, err)
		}
		if actual != expected {
			t.Fatalf("ParseRetentionDuration(%q) = %s, want %s", value, actual, expected)
		}
	}
}

func TestCleanupPolicyRequiresCriteria(t *testing.T) {
	policy := CleanupPolicy{
		Name:         "unsafe",
		Repositories: []string{"raw"},
	}
	if err := policy.Validate(); !errors.Is(err, ErrCleanupCriteriaRequired) {
		t.Fatalf("empty policy returned %v, want ErrCleanupCriteriaRequired", err)
	}
}

func TestCleanupPredicateValidation(t *testing.T) {
	valid := CleanupPolicy{
		Name:         "cleanup",
		Repositories: []string{"raw"},
		Criteria: CleanupCriteria{
			{Path: "sys.blobStore", Op: "=", Value: "archive"},
			{Path: "sys.updatedAt", Op: "before", Value: "6mo"},
			{Path: "scan.verified", Op: "exists"},
			{Path: "scan.result", Op: "=", Value: nil},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid predicates returned %v", err)
	}

	tests := []struct {
		name      string
		predicate Predicate
		expected  error
	}{
		{name: "path", predicate: Predicate{Path: "sys..size", Op: "=", Value: 1}, expected: ErrInvalidPredicatePath},
		{name: "operator", predicate: Predicate{Path: "sys.size", Op: "approximately", Value: 1}, expected: ErrInvalidPredicateOperator},
		{name: "regex", predicate: Predicate{Path: "sys.path", Op: "matches", Value: "["}, expected: ErrInvalidPredicatePattern},
		{name: "temporal literal", predicate: Predicate{Path: "sys.updatedAt", Op: "before", Value: "eventually"}, expected: ErrInvalidPredicateDuration},
		{name: "in literal", predicate: Predicate{Path: "sys.kind", Op: "in", Value: "raw"}, expected: ErrInvalidPredicateValue},
		{name: "exists value", predicate: Predicate{Path: "sys.kind", Op: "exists", Value: true}, expected: ErrInvalidPredicateValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := valid
			policy.Criteria = CleanupCriteria{test.predicate}
			if err := policy.Validate(); !errors.Is(err, test.expected) {
				t.Fatalf("Validate() = %v, want %v", err, test.expected)
			}
		})
	}
}

func TestWebhookValidation(t *testing.T) {
	valid := Webhook{
		Name:   "scanner",
		URL:    "https://scanner.example/hooks/suxen",
		Secret: "sufficiently-long-secret",
		Events: []string{WebhookAssetUploaded},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid webhook returned %v", err)
	}

	tests := []struct {
		name     string
		mutate   func(*Webhook)
		expected error
	}{
		{
			name: "target scheme",
			mutate: func(webhook *Webhook) {
				webhook.URL = "file:///tmp/hook"
			},
			expected: ErrInvalidWebhookURL,
		},
		{
			name: "short secret",
			mutate: func(webhook *Webhook) {
				webhook.Secret = "short"
			},
			expected: ErrWebhookSecretRequired,
		},
		{
			name: "event type",
			mutate: func(webhook *Webhook) {
				webhook.Events = []string{"asset.unknown"}
			},
			expected: ErrInvalidWebhookEvent,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			webhook := valid
			test.mutate(&webhook)
			if err := webhook.Validate(); !errors.Is(err, test.expected) {
				t.Fatalf("Validate() returned %v, want %v", err, test.expected)
			}
		})
	}
}
