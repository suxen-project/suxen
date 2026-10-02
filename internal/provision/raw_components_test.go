package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProvisionRawComponentsAndVersionOrder(t *testing.T) {
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	ctx := context.Background()
	document := `resources:
  - kind: repository
    name: models
    spec:
      format: raw
      type: hosted
      formatConfig:
        components:
          - pattern: '^(?P<name>(models|tracks)/.+)/(?P<version>[0-9][^/]*)/[^/]+$'
            anchor: '\.(glb|zip)$'
          - pattern: '^(?P<name>client/alpha/[^/]+)/trackmaniac-(?P<version>[^/]+)-[^/]+\.zip$'
  - kind: cleanupPolicy
    name: keep-latest-model
    spec:
      repositories: [models]
      criteria:
        - {path: raw.component, op: exists}
      keepLast: 1
      order: version
`
	report, err := engine.Apply(ctx, mustResolveDocument(t, document), Options{})
	if err != nil || report.Failed() {
		t.Fatalf("apply: report=%+v err=%v", report, err)
	}
	repository, err := metadata.Repository(ctx, "models")
	if err != nil {
		t.Fatal(err)
	}
	if components, _ := repository.FormatConfig["components"].([]any); len(components) != 2 {
		t.Fatalf("stored formatConfig = %v", repository.FormatConfig)
	}
	policy, err := metadata.CleanupPolicy(ctx, "keep-latest-model")
	if err != nil || policy.Order != domain.CleanupOrderVersion {
		t.Fatalf("stored policy = %+v, %v", policy, err)
	}
	report, err = engine.Apply(ctx, mustResolveDocument(t, document), Options{})
	if err != nil || report.Failed() {
		t.Fatalf("reapply: report=%+v err=%v", report, err)
	}
	for _, result := range report.Results {
		if result.Status != StatusUnchanged {
			t.Fatalf("reapply %s/%s = %s, want unchanged", result.Kind, result.Name, result.Status)
		}
	}
}

func TestProvisionRejectsRawComponentWithoutVersionGroup(t *testing.T) {
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	ctx := context.Background()
	report, err := engine.Apply(ctx, mustResolveDocument(t, `resources:
  - kind: repository
    name: models
    spec:
      format: raw
      type: hosted
      formatConfig:
        components:
          - pattern: '^(?P<name>.+)/[^/]+$'
`), Options{})
	if err == nil && !report.Failed() {
		t.Fatalf("invalid component pattern accepted: %+v", report)
	}
	if _, err := metadata.Repository(ctx, "models"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("repository with invalid components was stored: %v", err)
	}
}
