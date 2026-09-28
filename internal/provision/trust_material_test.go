package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// testTrustPublicKeyPEM is a syntactically valid PEM public key. Fixtures that
// provision a trust policy use it because provisioning preflight now validates
// trust material the same way the resource API does.
var testTrustPublicKeyPEM = mustTrustPublicKeyPEM()

func mustTrustPublicKeyPEM() string {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func resultByKind(report Report, kind, name string) (Result, bool) {
	for _, result := range report.Results {
		if result.Kind == kind && result.Name == name {
			return result, true
		}
	}
	return Result{}, false
}

// A repository trust policy with material the resource API would reject must
// also fail provisioning, during preflight, before any unrelated resource in the
// same document is mutated. This is the parity gap the leaf validator closes.
func TestProvisioningRejectsInvalidRepositoryTrustMaterial(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%t", dryRun), func(t *testing.T) {
			ctx := context.Background()
			metadata := newProvisionTestStore(t)
			document := mustResolveDocument(t, `
resources:
  - kind: role
    name: untouched
    spec: {privileges: [repository:app:read]}
  - kind: repository
    name: app
    spec: {format: raw, type: hosted}
  - kind: trustPolicy
    name: app
    spec: {mode: audit, publicKeys: ["not a PEM key"]}
`)
			report, err := (Engine{Store: metadata}).Apply(ctx, document, Options{DryRun: dryRun})
			if err != nil {
				t.Fatalf("Apply returned a hard error, want a failed report: %v", err)
			}
			if !report.Failed() {
				t.Fatalf("report did not fail on invalid trust material: %+v", report.Results)
			}
			result, found := resultByKind(report, "trustPolicy", "app")
			if !found || result.Status != StatusFailed {
				t.Fatalf("trustPolicy result = %+v (found %v), want failed", result, found)
			}
			if !strings.Contains(result.Error, domain.ErrInvalidTrustMaterial.Error()) {
				t.Fatalf("trustPolicy error = %q, want it to mention invalid trust material", result.Error)
			}
			// The rejection is a preflight failure, so no resource is persisted:
			// not the invalid policy, and not the unrelated role or repository
			// declared earlier in the same document.
			if _, err := metadata.Role(ctx, "untouched"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("unrelated role was mutated: %v", err)
			}
			if _, err := metadata.Repository(ctx, "app"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("repository was created before preflight completed: %v", err)
			}
			if _, err := metadata.TrustPolicy(ctx, "app"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("rejected trust policy was stored: %v", err)
			}
		})
	}
}

// The instance-wide trust-policy default flows through a different reconcile path
// and must reject invalid material on the same preflight terms.
func TestProvisioningRejectsInvalidTrustDefaultMaterial(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%t", dryRun), func(t *testing.T) {
			ctx := context.Background()
			metadata := newProvisionTestStore(t)
			document := mustResolveDocument(t, `
resources:
  - kind: trustPolicy
    name: default
    spec: {mode: audit, publicKeys: ["not a PEM key"]}
`)
			report, err := (Engine{Store: metadata}).Apply(ctx, document, Options{DryRun: dryRun})
			if err != nil {
				t.Fatalf("Apply returned a hard error, want a failed report: %v", err)
			}
			result, found := resultByKind(report, "trustPolicy", "default")
			if !found || result.Status != StatusFailed {
				t.Fatalf("trustPolicy default result = %+v (found %v), want failed", result, found)
			}
			if !strings.Contains(result.Error, domain.ErrInvalidTrustMaterial.Error()) {
				t.Fatalf("trustPolicy default error = %q, want invalid trust material", result.Error)
			}
			if _, err := metadata.TrustPolicyDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("rejected trust-policy default was stored: %v", err)
			}
		})
	}
}

// Valid PEM material still provisions through both the repository and instance
// default paths, so the parity fix does not reject legitimate policies.
func TestProvisioningAcceptsValidTrustMaterial(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	document := mustResolveDocument(t, fmt.Sprintf(`
resources:
  - kind: repository
    name: app
    spec: {format: raw, type: hosted}
  - kind: trustPolicy
    name: app
    spec: {mode: audit, publicKeys: [%q]}
  - kind: trustPolicy
    name: default
    spec: {mode: audit, publicKeys: [%q]}
`, testTrustPublicKeyPEM, testTrustPublicKeyPEM))
	report, err := (Engine{Store: metadata}).Apply(ctx, document, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed() {
		t.Fatalf("valid trust material failed to provision: %+v", report.Results)
	}
	if _, err := metadata.TrustPolicy(ctx, "app"); err != nil {
		t.Fatalf("repository trust policy not stored: %v", err)
	}
	if _, err := metadata.TrustPolicyDefaults(ctx); err != nil {
		t.Fatalf("instance trust-policy default not stored: %v", err)
	}
}
