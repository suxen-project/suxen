package provision

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestParseAndResolveSecretReferences(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "webhook-secret")
	if err := os.WriteFile(secretPath, []byte("file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVISION_TEST_PASSWORD", "environment-password")

	document, err := Parse(strings.NewReader(`
apiVersion: suxen.io/v1
resources:
  - kind: user
    name: automation
    spec:
      admin: false
      secretRef:
        env: PROVISION_TEST_PASSWORD
  - kind: webhook
    name: notifications
    spec:
      url: https://hooks.example.test/suxen
      events: [asset.uploaded]
      secretRef:
        file: ` + secretPath + `
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveSecrets(document, EnvironmentResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resources[0].Secret != "environment-password" {
		t.Fatalf("user secret was not resolved")
	}
	if resolved.Resources[1].Secret != "file-secret-value" {
		t.Fatalf("webhook secret was not resolved")
	}
	for _, resource := range resolved.Resources {
		if _, retained := resource.Spec["secretRef"]; retained {
			t.Fatalf("resolved spec retained secretRef")
		}
		if _, retained := resource.Spec["__secretRef"]; retained {
			t.Fatalf("resolved spec retained internal secret reference")
		}
	}
}

func TestParseRejectsDuplicateAndAmbiguousSecrets(t *testing.T) {
	tests := []struct {
		name     string
		document string
		contains string
	}{
		{
			name: "duplicate",
			document: `
resources:
  - kind: role
    name: reader
    spec: {}
  - kind: roles
    name: reader
    spec: {}
`,
			contains: "duplicate role",
		},
		{
			name: "inline and reference",
			document: `
resources:
  - kind: user
    name: automation
    spec:
      password: inline-password
      secretRef:
        env: AUTOMATION_PASSWORD
`,
			contains: "mutually exclusive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(test.document))
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want text %q", err, test.contains)
			}
		})
	}
}

func TestEnvironmentResolverRequiresAbsoluteFile(t *testing.T) {
	_, err := (EnvironmentResolver{}).Resolve(SecretReference{File: "relative-secret"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want absolute path validation", err)
	}
}

func TestParseAllowsEmptyDesiredStateForExplicitPrune(t *testing.T) {
	document, err := Parse(strings.NewReader("apiVersion: suxen.io/v1\nresources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Resources) != 0 {
		t.Fatalf("resources = %d, want zero", len(document.Resources))
	}
}

func TestParseJSONLines(t *testing.T) {
	document, err := Parse(strings.NewReader(
		"{\"kind\":\"role\",\"name\":\"reader\",\"spec\":{\"privileges\":[\"repository:*:read\"]}}\n" +
			"{\"kind\":\"role\",\"name\":\"writer\",\"spec\":{\"privileges\":[\"repository:*:write\"]}}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Resources) != 2 || document.Resources[1].Name != "writer" {
		t.Fatalf("resources = %+v", document.Resources)
	}
}

func TestParseRejectsDocumentLargerThanLimit(t *testing.T) {
	oversized := strings.Repeat(" ", MaxDocumentSize+1)
	_, err := Parse(strings.NewReader(oversized))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want size limit", err)
	}
}

func TestParseRejectsUnknownDocumentAndResourceFields(t *testing.T) {
	tests := []string{
		"apiVersion: suxen.io/v1\nresources: []\nprune: true\n",
		"resources:\n  - kind: role\n    name: reader\n    specs: {}\n",
	}
	for _, contents := range tests {
		_, err := Parse(strings.NewReader(contents))
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("Parse() error = %v, want unknown field rejection", err)
		}
	}
}

func TestParseRejectsUnknownSecretReferenceFields(t *testing.T) {
	_, err := Parse(strings.NewReader(`
resources:
  - kind: user
    name: automation
    spec:
      secretRef:
        env: AUTOMATION_PASSWORD
        typo: rejected
`))
	if err == nil || !strings.Contains(err.Error(), "unknown field \"typo\"") {
		t.Fatalf("Parse() error = %v, want strict secretRef rejection", err)
	}
}

func TestResolveSecretsRejectsTrailingReferenceData(t *testing.T) {
	document := Document{
		APIVersion: APIVersion,
		Resources: []Resource{
			{
				Kind: "user",
				Name: "automation",
				Spec: map[string]any{
					"__secretRef": `{"env":"AUTOMATION_PASSWORD"} {}`,
				},
			},
		},
	}
	_, err := ResolveSecrets(document, EnvironmentResolver{})
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("ResolveSecrets() error = %v, want trailing data rejection", err)
	}
}

func TestParseCanonicalRequiresEnvelopeAndCanonicalKinds(t *testing.T) {
	tests := []string{
		"- kind: role\n  name: reader\n  spec: {}\n",
		"resources:\n  - kind: roles\n    name: reader\n    spec: {}\n",
	}
	for _, contents := range tests {
		if _, err := ParseCanonical(strings.NewReader(contents)); err == nil {
			t.Fatalf("ParseCanonical(%q) unexpectedly succeeded", contents)
		}
		if _, err := Parse(strings.NewReader(contents)); err != nil {
			t.Fatalf("flexible Parse(%q) failed: %v", contents, err)
		}
	}
}

func TestParseCanonicalAllowsOmittedOrNullSpec(t *testing.T) {
	for _, contents := range []string{
		"resources:\n  - kind: role\n    name: reader\n",
		"resources:\n  - kind: role\n    name: reader\n    spec: null\n",
	} {
		document, err := ParseCanonical(strings.NewReader(contents))
		if err != nil {
			t.Fatal(err)
		}
		if document.Resources[0].Spec == nil || len(document.Resources[0].Spec) != 0 {
			t.Fatalf("spec = %#v, want empty map", document.Resources[0].Spec)
		}
	}
}

func TestParseCanonicalValidatesUsernames(t *testing.T) {
	document, err := ParseCanonical(strings.NewReader(`
resources:
  - kind: user
    name: release.bot@example.com
`))
	if err != nil {
		t.Fatal(err)
	}
	if document.Resources[0].Name != "release.bot@example.com" {
		t.Fatalf("username = %q", document.Resources[0].Name)
	}

	_, err = ParseCanonical(strings.NewReader("resources:\n  - kind: user\n    name: ''\n"))
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("empty username error = %v", err)
	}
	for _, username := range []string{"ops/team", "ops:team", "ops team", " ops", ".", "équipe"} {
		_, err := ParseCanonical(strings.NewReader("resources:\n  - kind: user\n    name: '" + username + "'\n"))
		if !errors.Is(err, domain.ErrInvalidUsername) {
			t.Errorf("username %q error = %v, want ErrInvalidUsername", username, err)
		}
	}
}
