package server

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

var uiSchemaOpenAPINames = map[string]string{
	"repository":             "RepositoryCreateRequest",
	"blobStore":              "BlobStoreCreateRequest",
	"cleanupPolicy":          "CleanupPolicyCreateRequest",
	"user":                   "UserCreateRequest",
	"role":                   "RoleCreateRequest",
	"oidc":                   "OIDCProviderCreateRequest",
	"webhook":                "WebhookRequest",
	"classification":         "ClassificationConfigRequest",
	"downloadGate":           "DownloadGateRequest",
	"trustPolicy":            "TrustPolicyRequest",
	"classificationDefaults": "ClassificationDefaultsUpdateRequest",
	"downloadGateDefaults":   "DownloadGateDefaultsUpdateRequest",
	"trustPolicyDefaults":    "TrustPolicyRequest",
	"verification":           "VerificationRequest",
	"token":                  "TokenCreateRequest",
}

func TestUISchemasMatchOpenAPIRequestFields(t *testing.T) {
	source, err := os.ReadFile("ui/schemas.js")
	if err != nil {
		t.Fatalf("read ui/schemas.js: %v", err)
	}
	document := decodeOpenAPITemplate(t)
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	uiSchemas := parseUISchemaFields(t, string(source))

	wantEvents := []string{
		domain.WebhookAssetUploaded,
		domain.WebhookAssetDeleted,
		domain.WebhookAssetDownloaded,
		domain.WebhookComponentCreated,
		domain.WebhookCleanupCompleted,
	}
	for _, event := range wantEvents {
		if !domain.IsWebhookEventType(event) {
			t.Errorf("domain constant %q is not a webhook event type", event)
		}
	}
	gotEvents := parseUIWebhookEvents(t, string(source))
	if fmt.Sprint(gotEvents) != fmt.Sprint(wantEvents) {
		t.Errorf("ui webhookEvents = %v, want %v", gotEvents, wantEvents)
	}
	webhookRequest := objectValue(t, schemas, "WebhookRequest")
	openAPIEvents := stringSlice(
		t,
		objectValue(
			t,
			objectValue(t, objectValue(t, webhookRequest, "properties"), "events"),
			"items",
		)["enum"],
	)
	if fmt.Sprint(openAPIEvents) != fmt.Sprint(wantEvents) {
		t.Errorf("OpenAPI WebhookRequest events = %v, want %v", openAPIEvents, wantEvents)
	}

	for uiKey, schemaName := range uiSchemaOpenAPINames {
		fields, found := uiSchemas[uiKey]
		if !found {
			t.Errorf("ui schema %q is missing", uiKey)
			continue
		}
		properties := schemaPropertyNames(t, document, objectValue(t, schemas, schemaName))
		for _, field := range fields {
			if _, found := properties[field]; !found {
				t.Errorf("ui schema %s field %q is not on OpenAPI %s", uiKey, field, schemaName)
			}
		}
	}

	if _, found := uiSchemas["attributes"]; !found {
		t.Fatal("ui attributes schema is missing")
	}
	for _, field := range uiSchemas["attributes"] {
		if field != "namespace" && field != "value" {
			t.Errorf("ui attributes field %q is not the client envelope", field)
		}
	}
}

func parseUIWebhookEvents(t *testing.T, source string) []string {
	t.Helper()
	block := regexp.MustCompile(`export const webhookEvents = \[([\s\S]*?)\];`)
	match := block.FindStringSubmatch(source)
	if match == nil {
		t.Fatal("webhookEvents array not found")
	}
	matches := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(match[1], -1)
	events := make([]string, 0, len(matches))
	for _, item := range matches {
		events = append(events, item[1])
	}
	return events
}

func parseUISchemaFields(t *testing.T, source string) map[string][]string {
	t.Helper()
	block := regexp.MustCompile(`export const schemas = \{([\s\S]*?)\n\};`)
	match := block.FindStringSubmatch(source)
	if match == nil {
		t.Fatal("schemas object not found")
	}
	headers := regexp.MustCompile(`(?m)^  ([A-Za-z]+): \[`).FindAllStringSubmatchIndex(match[1], -1)
	if len(headers) == 0 {
		t.Fatal("no ui schema keys found")
	}
	namePattern := regexp.MustCompile(`(?:name:\s*|(?:text|list|json)\()\s*"([^"]+)"`)
	fields := make(map[string][]string)
	body := match[1]
	for index, loc := range headers {
		name := body[loc[2]:loc[3]]
		start := loc[1]
		end := len(body)
		if index+1 < len(headers) {
			end = headers[index+1][0]
		}
		section := body[start:end]
		for _, field := range namePattern.FindAllStringSubmatch(section, -1) {
			fields[name] = append(fields[name], field[1])
		}
	}
	return fields
}

func schemaPropertyNames(t *testing.T, document map[string]any, schema any) map[string]struct{} {
	t.Helper()
	resolved := resolveReference(t, document, schema).(map[string]any)
	names := make(map[string]struct{})
	if properties, ok := resolved["properties"].(map[string]any); ok {
		for name := range properties {
			names[name] = struct{}{}
		}
	}
	for _, composition := range []string{"allOf", "oneOf", "anyOf"} {
		rawParts, ok := resolved[composition].([]any)
		if !ok {
			continue
		}
		for _, part := range rawParts {
			for name := range schemaPropertyNames(t, document, part) {
				names[name] = struct{}{}
			}
		}
	}
	return names
}
