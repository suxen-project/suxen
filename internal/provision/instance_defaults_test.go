package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// The instance-wide default of a classification, trustPolicy, or downloadGate is
// addressed on the base kind by the reserved name "default"; there is no separate
// *Defaults kind. The trust policy carries real PEM material because provisioning
// preflight now validates it.
var instanceDefaultsDocument = fmt.Sprintf(`
resources:
  - kind: downloadGate
    name: default
    spec:
      criteria:
        - {path: scan.status, op: "=", value: passed}
      enabled: true
  - kind: classification
    name: default
    spec:
      rules:
        - {when: [], key: tier, value: public}
  - kind: trustPolicy
    name: default
    spec:
      mode: audit
      publicKeys: [%q]
`, testTrustPublicKeyPEM)

func statusByKind(report Report, kind string) (string, bool) {
	for _, result := range report.Results {
		if result.Kind == kind {
			return result.Status, true
		}
	}
	return "", false
}

func TestEngineProvisionsInstanceDefaults(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	report, err := engine.Apply(ctx, mustResolveDocument(t, instanceDefaultsDocument), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"downloadGate", "classification", "trustPolicy"} {
		status, found := statusByKind(report, kind)
		if !found || status != StatusCreated {
			t.Fatalf("%s status = %q (found %v), want %s", kind, status, found, StatusCreated)
		}
		// Provisioning ownership makes the default managed and read-only at runtime.
		if _, err := metadata.ProvisionRecord(ctx, kind, domain.InstanceDefaultsName); err != nil {
			t.Fatalf("%s should have a provisioning record: %v", kind, err)
		}
	}

	gate, err := metadata.DownloadGateDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Enabled || len(gate.Criteria) != 1 {
		t.Fatalf("download-gate default not stored: %+v", gate)
	}
	config, err := metadata.ClassificationDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Rules) != 1 || config.Rules[0].Key != "tier" {
		t.Fatalf("classification default not stored: %+v", config)
	}
	policy, err := metadata.TrustPolicyDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != "audit" {
		t.Fatalf("trust-policy default not stored: %+v", policy)
	}

	second, err := engine.Apply(ctx, mustResolveDocument(t, instanceDefaultsDocument), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"downloadGate", "classification", "trustPolicy"} {
		if status, _ := statusByKind(second, kind); status != StatusUnchanged {
			t.Fatalf("%s re-apply status = %q, want %s", kind, status, StatusUnchanged)
		}
	}
}

// A document may declare the instance default at most once per kind: the reserved
// (kind, "default") key makes a second declaration a duplicate resource.
func TestEngineRejectsDuplicateInstanceDefault(t *testing.T) {
	_, err := Parse(strings.NewReader(`
resources:
  - kind: classification
    name: default
    spec:
      rules:
        - {when: [], key: tier, value: public}
  - kind: classification
    name: default
    spec:
      rules:
        - {when: [], key: tier, value: private}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate classification \"default\"") {
		t.Fatalf("duplicate instance default error = %v, want duplicate classification \"default\"", err)
	}
}

func TestEnginePrunesInstanceDefaults(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	if _, err := engine.Apply(ctx, mustResolveDocument(t, instanceDefaultsDocument), Options{}); err != nil {
		t.Fatal(err)
	}

	pruned, err := engine.Apply(ctx, Document{APIVersion: APIVersion}, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"downloadGate", "classification", "trustPolicy"} {
		if status, found := statusByKind(pruned, kind); !found || status != StatusDeleted {
			t.Fatalf("%s prune status = %q (found %v), want %s", kind, status, found, StatusDeleted)
		}
		if _, err := metadata.ProvisionRecord(ctx, kind, domain.InstanceDefaultsName); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s provisioning record should be gone: %v", kind, err)
		}
	}
	if _, err := metadata.DownloadGateDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("download-gate default should be cleared: %v", err)
	}
	if _, err := metadata.TrustPolicyDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trust-policy default should be cleared: %v", err)
	}
}

// A default first created at runtime (no provisioning record) is adopted by a
// provisioning document that declares the same content: the value stays, and the
// resource becomes managed.
func TestEngineAdoptsUnmanagedInstanceDefault(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}

	if err := metadata.SetTrustPolicyDefaults(ctx, domain.TrustPolicy{
		Mode:       "audit",
		PublicKeys: []string{testTrustPublicKeyPEM},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ProvisionRecord(ctx, "trustPolicy", domain.InstanceDefaultsName); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("runtime default must start unmanaged: %v", err)
	}

	report, err := engine.Apply(ctx, mustResolveDocument(t, fmt.Sprintf(`
resources:
  - kind: trustPolicy
    name: default
    spec:
      mode: audit
      publicKeys: [%q]
`, testTrustPublicKeyPEM)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := statusByKind(report, "trustPolicy"); status != StatusUpdated {
		t.Fatalf("adopting an unmanaged default status = %q, want %s", status, StatusUpdated)
	}
	if _, err := metadata.ProvisionRecord(ctx, "trustPolicy", domain.InstanceDefaultsName); err != nil {
		t.Fatalf("adopted default should be managed: %v", err)
	}
}
