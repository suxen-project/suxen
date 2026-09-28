package oci

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestUploadSessionPolicyDefaultsAndOverrides(t *testing.T) {
	defaults := uploadSessionLimits(domain.BlobStore{}, 128)
	if defaults.MaxStagedBytes != 128 || defaults.MaxPrincipalStagedBytes != 128 ||
		defaults.MaxPrincipalSessions != 4 {
		t.Fatalf("unexpected finite defaults: %+v", defaults)
	}
	if stale := uploadSessionStaleAfter(domain.BlobStore{}); stale != defaultUploadSessionStaleAfter {
		t.Fatalf("default stale interval = %v, want %v", stale, defaultUploadSessionStaleAfter)
	}

	resource := domain.BlobStore{Attributes: map[string]any{
		"uploadSessions": map[string]any{
			"staleAfter":              "15m",
			"maxStagedBytes":          float64(64),
			"maxPrincipalStagedBytes": float64(32),
			"maxPrincipalSessions":    float64(2),
		},
	}}
	overrides := uploadSessionLimits(resource, 128)
	if overrides.MaxStagedBytes != 64 || overrides.MaxPrincipalStagedBytes != 32 ||
		overrides.MaxPrincipalSessions != 2 {
		t.Fatalf("unexpected policy overrides: %+v", overrides)
	}
	if stale := uploadSessionStaleAfter(resource); stale.String() != "15m0s" {
		t.Fatalf("stale interval = %v, want 15m", stale)
	}
}

func TestUploadSessionPolicyExactJSONOverrides(t *testing.T) {
	resource := domain.BlobStore{Name: "constrained", Driver: "fs", ConfigurationRef: &domain.ConfigurationReference{Env: "CONSTRAINED_STORE"}, PhysicalIdentity: strings.Repeat("a", 64), Attributes: map[string]any{
		"uploadSessions": map[string]any{
			"maxStagedBytes":          json.Number("1e2"),
			"maxPrincipalStagedBytes": json.Number("100.0"),
			"maxPrincipalSessions":    json.Number("1e1"),
		},
	}}
	if err := resource.Validate(); err != nil {
		t.Fatalf("valid upload-session policy rejected: %v", err)
	}
	limits := uploadSessionLimits(resource, 128)
	if limits.MaxStagedBytes != 100 || limits.MaxPrincipalStagedBytes != 100 || limits.MaxPrincipalSessions != 10 {
		t.Fatalf("exact JSON limits were not applied: %+v", limits)
	}
}
