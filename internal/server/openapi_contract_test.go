package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/contract"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

var openAPIMethods = map[string]struct{}{
	"delete":  {},
	"get":     {},
	"head":    {},
	"options": {},
	"patch":   {},
	"post":    {},
	"put":     {},
	"trace":   {},
}

func TestTrustPolicyContractRejectsGroups(t *testing.T) {
	t.Parallel()
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromData(openAPITemplate)
	if err != nil {
		t.Fatal(err)
	}
	operation := document.Paths.Value("/api/v1/repositories/{name}/trust-policy").Put
	if operation.Responses.Value("400") == nil {
		t.Fatal("group policy rejection must be documented as a bad request")
	}
	fixture := newServerFixture(t)
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "group", Format: "raw", Type: "group", Members: []string{"raw"},
	}); err != nil {
		t.Fatal(err)
	}
	_, key := provenanceTestKey(t)
	for _, mode := range []string{"audit", "verify-on-pull", "verify-on-push"} {
		t.Run(mode, func(t *testing.T) {
			body, err := json.Marshal(trustPolicyRequest{Mode: mode, PublicKeys: []string{key}})
			if err != nil {
				t.Fatal(err)
			}
			response := fixture.requestWithBearer(t, http.MethodPut,
				"/api/v1/repositories/group/trust-policy", body, "application/json", testToken)
			defer response.Body.Close()
			assertStatus(t, response, http.StatusBadRequest)
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != "invalid_trust_policy" {
				t.Fatalf("error code = %q, want invalid_trust_policy", problem.Code)
			}
		})
	}
	response := fixture.request(t, http.MethodGet, "/api/v1/repositories/group/trust-policy", nil, true)
	assertStatus(t, response, http.StatusNotFound)
	response.Body.Close()
}

func TestOpenAPIContractMatchesControlPlaneRoutes(t *testing.T) {
	document := decodeOpenAPITemplate(t)
	documented := documentedOperations(t, document)
	registered := registeredOperations(t)

	for operation := range registered {
		if _, found := documented[operation]; !found {
			t.Errorf("registered operation %s is missing from OpenAPI", operation)
		}
	}
	for operation := range documented {
		if _, found := registered[operation]; !found {
			t.Errorf("OpenAPI operation %s has no registered route", operation)
		}
	}
}

func TestOpenAPIContractPassesOpenAPIValidation(t *testing.T) {
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromData(openAPITemplate)
	if err != nil {
		t.Fatalf("load embedded OpenAPI contract: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate embedded OpenAPI contract: %v", err)
	}
}

func TestAPIDiscoverySchemaDocumentsContractVersions(t *testing.T) {
	document := decodeOpenAPITemplate(t)
	get := objectValue(t, objectValue(t, objectValue(t, document, "paths"), "/api/v1"), "get")
	responses := objectValue(t, objectValue(t, get, "responses"), "200")
	schema := objectValue(t,
		objectValue(t, objectValue(t, responses, "content"), "application/json"),
		"schema",
	)

	if !requiredContains(arrayValue(t, schema, "required"), "contract") {
		t.Error("GET /api/v1 schema does not require the contract matrix field")
	}
	properties := objectValue(t, schema, "properties")

	versionsItems := objectValue(t, objectValue(t, properties, "versions"), "items")
	if !requiredContains(arrayValue(t, versionsItems, "required"), "version") {
		t.Error("versions[] items schema does not require the version field")
	}
	if _, found := objectValue(t, versionsItems, "properties")["version"]; !found {
		t.Error("versions[] items schema does not declare the version property")
	}

	contract := objectValue(t, properties, "contract")
	if kind := stringValue(contract, "type"); kind != "array" {
		t.Errorf("contract schema type = %q, want array", kind)
	}
	contractItems := objectValue(t, contract, "items")
	contractRequired := arrayValue(t, contractItems, "required")
	contractProperties := objectValue(t, contractItems, "properties")
	for _, field := range []string{"id", "version"} {
		if !requiredContains(contractRequired, field) {
			t.Errorf("contract items schema does not require %q", field)
		}
		if _, found := contractProperties[field]; !found {
			t.Errorf("contract items schema does not declare property %q", field)
		}
	}
}

func requiredContains(required []any, field string) bool {
	for _, value := range required {
		if name, ok := value.(string); ok && name == field {
			return true
		}
	}
	return false
}

func TestProvisioningUserSchemaAcceptsRuntimeUsernames(t *testing.T) {
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromData(openAPITemplate)
	if err != nil {
		t.Fatal(err)
	}
	schema := document.Components.Schemas["ProvisioningUserResource"].Value
	if err := schema.VisitJSON(map[string]any{
		"kind": "user",
		"name": "release.bot@example.com",
	}); err != nil {
		t.Fatalf("validate arbitrary non-empty username: %v", err)
	}
	if err := schema.VisitJSON(map[string]any{
		"kind": "user",
		"name": "",
	}); err == nil {
		t.Fatal("empty provisioning username unexpectedly passed schema validation")
	}
}

func TestOpenAPIContractIsStructurallyComplete(t *testing.T) {
	document := decodeOpenAPITemplate(t)

	if version := stringValue(document, "openapi"); !strings.HasPrefix(version, "3.") {
		t.Fatalf("openapi version %q is not OpenAPI 3.x", version)
	}
	components := objectValue(t, document, "components")
	securitySchemes := objectValue(t, components, "securitySchemes")
	for _, name := range []string{"basicAuth", "bearerAuth", "oidcSession"} {
		if _, found := securitySchemes[name]; !found {
			t.Errorf("security scheme %q is missing", name)
		}
	}
	if len(arrayValue(t, document, "security")) != 4 {
		t.Fatal("global security must describe anonymous, Basic, Bearer, and OIDC session principals")
	}

	operationIDs := make(map[string]string)
	paths := objectValue(t, document, "paths")
	for path, rawPathItem := range paths {
		pathItem := rawPathItem.(map[string]any)
		for method, rawOperation := range pathItem {
			if _, isMethod := openAPIMethods[method]; !isMethod {
				continue
			}
			operation := rawOperation.(map[string]any)
			operationID := stringValue(operation, "operationId")
			if operationID == "" {
				t.Errorf("%s %s has no operationId", strings.ToUpper(method), path)
			} else if previous, duplicate := operationIDs[operationID]; duplicate {
				t.Errorf("operationId %q is shared by %s and %s %s", operationID, previous, strings.ToUpper(method), path)
			} else {
				operationIDs[operationID] = strings.ToUpper(method) + " " + path
			}
			if stringValue(operation, "summary") == "" {
				t.Errorf("%s %s has no summary", strings.ToUpper(method), path)
			}
			responses := objectValue(t, operation, "responses")
			if len(responses) == 0 || !hasSuccessResponse(responses) {
				t.Errorf("%s %s must document a successful response", strings.ToUpper(method), path)
			}
			assertPathParameters(t, document, path, pathItem, operation)
		}
	}

	assertAllLocalReferencesResolve(t, document)
	assertRequestBodies(t, document)
	assertAssetContract(t, document)
	assertStablePredicateSchema(t, document)
	assertProblemDetailsContract(t, document)
	assertBlobStoreContract(t, document)
	assertRepositoryFormatConfigContract(t, document)
	assertCollectionPaginationContract(t, document)
	assertResponseFieldContract(t, document)
	assertCreatedResponsesDeclareLocation(t, document)
	assertPrivilegeRequirements(t, document)
	assertPathAuthoritativeRequestContracts(t, document)
	assertNamedWebhookContract(t, document)
	assertProvisioningContract(t, document)
	assertProvisioningOwnershipContract(t, document)
}

func assertRepositoryFormatConfigContract(t *testing.T, document map[string]any) {
	t.Helper()
	const formatConfigReference = "#/components/schemas/RepositoryFormatConfig"
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	for _, schemaName := range []string{
		"RepositoryCreateRequest",
		"RepositoryUpdateRequest",
		"ProvisioningRepositorySpec",
	} {
		schema := objectValue(t, schemas, schemaName)
		properties := objectValue(t, schema, "properties")
		formatConfig, found := properties["formatConfig"]
		if !found {
			t.Errorf("%s does not document formatConfig", schemaName)
			continue
		}
		reference := stringValue(formatConfig.(map[string]any), "$ref")
		if reference != formatConfigReference {
			t.Errorf("%s formatConfig reference = %q", schemaName, reference)
		}
		endpoints, found := properties["endpoints"]
		if !found {
			t.Errorf("%s does not document endpoints", schemaName)
			continue
		}
		reference = stringValue(endpoints.(map[string]any), "$ref")
		if reference != "#/components/schemas/RepositoryEndpoints" {
			t.Errorf("%s endpoints reference = %q", schemaName, reference)
		}
	}
}

func assertPrivilegeRequirements(t *testing.T, document map[string]any) {
	t.Helper()
	paths := objectValue(t, document, "paths")
	for routePath, rawPathItem := range paths {
		pathItem := rawPathItem.(map[string]any)
		parts := httpx.SplitPath(strings.TrimPrefix(routePath, "/api/v1"))
		for method, rawOperation := range pathItem {
			if _, isMethod := openAPIMethods[method]; !isMethod {
				continue
			}
			operation := rawOperation.(map[string]any)
			got := stringValue(operation, "x-suxen-required-privilege")
			want := controlPlanePrivilegeRequirement(parts, strings.ToUpper(method))
			if got != want {
				t.Errorf(
					"%s %s privilege requirement = %q, want %q",
					strings.ToUpper(method),
					routePath,
					got,
					want,
				)
			}
		}
	}
}

func assertCreatedResponsesDeclareLocation(t *testing.T, document map[string]any) {
	t.Helper()
	paths := objectValue(t, document, "paths")
	for routePath, rawPathItem := range paths {
		pathItem := rawPathItem.(map[string]any)
		for method, rawOperation := range pathItem {
			if _, isMethod := openAPIMethods[method]; !isMethod {
				continue
			}
			operation := rawOperation.(map[string]any)
			responses := objectValue(t, operation, "responses")
			rawCreated, found := responses["201"]
			if !found {
				continue
			}
			created := resolveReference(t, document, rawCreated).(map[string]any)
			headers := objectValue(t, created, "headers")
			if _, found := headers["Location"]; !found {
				t.Errorf(
					"%s %s 201 response does not declare Location",
					strings.ToUpper(method),
					routePath,
				)
			}
		}
	}
}

func TestOpenAPIEndpointServesEmbeddedContract(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/openapi.json", nil, false)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()

	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var document map[string]any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode served OpenAPI document: %v", err)
	}
	if want := contract.HTTPAPIVersion(); stringValue(objectValue(t, document, "info"), "version") != want {
		t.Fatalf(
			"served API version = %q, want http-api contract version %q",
			stringValue(objectValue(t, document, "info"), "version"), want,
		)
	}
	for path := range objectValue(t, document, "paths") {
		if path != "/api/v1" && !strings.HasPrefix(path, "/api/v1/") {
			t.Errorf("control-plane contract unexpectedly contains data-plane path %q", path)
		}
	}
}

func TestControlPlaneRouteMatchingUsesCanonicalTemplates(t *testing.T) {
	for _, expected := range controlPlaneRoutes {
		actualPath := expected.path
		actualPath = strings.ReplaceAll(actualPath, "{name}", "example")
		actualPath = strings.ReplaceAll(actualPath, "{id}", "42")
		actualPath = strings.ReplaceAll(actualPath, "{namespace}", "scanner")

		route, _, found := matchControlPlaneRoute(httpx.SplitPath(strings.TrimPrefix(actualPath, "/api/v1")))
		if !found {
			t.Errorf("route template %q does not match its representative path", expected.path)
			continue
		}
		if route.path != expected.path {
			t.Errorf("path %q resolves to %q, want %q", actualPath, route.path, expected.path)
		}
	}
}

func TestControlPlaneUsesCanonicalResourceNames(t *testing.T) {
	registered := make(map[string]struct{}, len(controlPlaneRoutes))
	for _, route := range controlPlaneRoutes {
		registered[route.path] = struct{}{}
	}

	canonical := []string{
		"/api/v1/blob-stores",
		"/api/v1/blob-stores/{name}",
		"/api/v1/blob-stores/{name}/drain",
		"/api/v1/oidc-providers",
		"/api/v1/oidc-providers/{name}",
		"/api/v1/cleanup-policies",
		"/api/v1/gc",
		"/api/v1/verify",
		"/api/v1/repositories/{name}/classification",
		"/api/v1/repositories/{name}/download-gate",
		"/api/v1/repositories/{name}/trust-policy",
	}
	for _, routePath := range canonical {
		if _, found := registered[routePath]; !found {
			t.Errorf("canonical route %q is not registered", routePath)
		}
	}

}

func TestRegisteredControlPlaneMethodsReachHandlers(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	for _, route := range controlPlaneRoutes {
		requestPath := representativeRoutePath(route.path)
		for _, method := range route.methods {
			response := fixture.request(
				t,
				method,
				requestPath,
				[]byte("{}"),
				true,
			)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatalf("read %s %s response: %v", method, requestPath, err)
			}
			if response.StatusCode == http.StatusMethodNotAllowed {
				t.Errorf("registered operation %s %s returned method not allowed", method, route.path)
			}
			if response.StatusCode == http.StatusNotFound &&
				strings.Contains(string(body), "admin route not found") {
				t.Errorf("registered operation %s %s did not reach a handler", method, route.path)
			}
		}
	}
}

func decodeOpenAPITemplate(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(openAPITemplate, &document); err != nil {
		t.Fatalf("parse embedded OpenAPI JSON: %v", err)
	}
	return document
}

func registeredOperations(t *testing.T) map[string]struct{} {
	t.Helper()
	operations := make(map[string]struct{})
	for _, route := range controlPlaneRoutes {
		if route.path != "/api/v1" && !strings.HasPrefix(route.path, "/api/v1/") {
			t.Errorf("control-plane route %q is outside /api/v1", route.path)
		}
		for _, method := range route.methods {
			key := strings.ToUpper(method) + " " + route.path
			if _, duplicate := operations[key]; duplicate {
				t.Errorf("control-plane operation %s is registered more than once", key)
			}
			operations[key] = struct{}{}
		}
	}
	return operations
}

func documentedOperations(t *testing.T, document map[string]any) map[string]struct{} {
	t.Helper()
	operations := make(map[string]struct{})
	for path, rawPathItem := range objectValue(t, document, "paths") {
		pathItem, ok := rawPathItem.(map[string]any)
		if !ok {
			t.Fatalf("path item %q is not an object", path)
		}
		for method := range pathItem {
			if _, isMethod := openAPIMethods[method]; isMethod {
				operations[strings.ToUpper(method)+" "+path] = struct{}{}
			}
		}
	}
	return operations
}

func assertPathParameters(
	t *testing.T,
	document map[string]any,
	path string,
	pathItem map[string]any,
	operation map[string]any,
) {
	t.Helper()
	want := placeholderNames(path)
	got := make(map[string]struct{})
	for _, owner := range []map[string]any{pathItem, operation} {
		rawParameters, found := owner["parameters"]
		if !found {
			continue
		}
		for _, rawParameter := range rawParameters.([]any) {
			parameter := resolveReference(t, document, rawParameter).(map[string]any)
			if stringValue(parameter, "in") == "path" {
				got[stringValue(parameter, "name")] = struct{}{}
				if required, _ := parameter["required"].(bool); !required {
					t.Errorf("path parameter %q on %s is not required", stringValue(parameter, "name"), path)
				}
			}
		}
	}
	if fmt.Sprint(sortedKeys(got)) != fmt.Sprint(sortedKeys(want)) {
		t.Errorf("path parameters for %s = %v, want %v", path, sortedKeys(got), sortedKeys(want))
	}
}

func assertRequestBodies(t *testing.T, document map[string]any) {
	t.Helper()
	requiredBodies := map[string]struct{}{
		"POST /api/v1/provision":                                             {},
		"POST /api/v1/blob-stores":                                           {},
		"PUT /api/v1/blob-stores/{name}":                                     {},
		"POST /api/v1/repositories":                                          {},
		"PUT /api/v1/repositories/{name}":                                    {},
		"PUT /api/v1/repositories/{name}/assets/{id}/attributes/{namespace}": {},
		"POST /api/v1/repositories/{name}/assets/{id}/verification":          {},
		"PUT /api/v1/repositories/{name}/classification":                     {},
		"PUT /api/v1/repositories/{name}/download-gate":                      {},
		"PUT /api/v1/repositories/{name}/trust-policy":                       {},
		"POST /api/v1/users":                                                 {},
		"PUT /api/v1/users/{name}":                                           {},
		"PUT /api/v1/users/{name}/roles":                                     {},
		"POST /api/v1/roles":                                                 {},
		"PUT /api/v1/roles/{name}":                                           {},
		"POST /api/v1/oidc-providers":                                        {},
		"PUT /api/v1/oidc-providers/{name}":                                  {},
		"POST /api/v1/cleanup-policies":                                      {},
		"PUT /api/v1/cleanup-policies/{name}":                                {},
		"POST /api/v1/webhooks":                                              {},
		"PUT /api/v1/webhooks/{name}":                                        {},
	}
	paths := objectValue(t, document, "paths")
	for operationKey := range requiredBodies {
		method, path, _ := strings.Cut(operationKey, " ")
		operation := objectValue(t, paths[path].(map[string]any), strings.ToLower(method))
		body, found := operation["requestBody"]
		if !found {
			t.Errorf("%s is missing its JSON request body", operationKey)
			continue
		}
		resolved := resolveReference(t, document, body).(map[string]any)
		if required, _ := resolved["required"].(bool); !required {
			t.Errorf("%s request body is not required", operationKey)
		}
		content := objectValue(t, resolved, "content")
		mediaType := objectValue(t, content, "application/json")
		schema, found := mediaType["schema"]
		if !found {
			t.Errorf("%s request body has no schema", operationKey)
			continue
		}
		if operationKey != "PUT /api/v1/repositories/{name}/assets/{id}/attributes/{namespace}" {
			assertClosedRequestSchema(t, document, schema, operationKey)
		}
	}
}

func assertProvisioningContract(t *testing.T, document map[string]any) {
	t.Helper()
	paths := objectValue(t, document, "paths")
	operation := objectValue(t, objectValue(t, paths, "/api/v1/provision"), "post")
	if privilege := stringValue(operation, "x-suxen-required-privilege"); privilege != "admin:provision:write" {
		t.Errorf("provisioning privilege = %q, want admin:provision:write", privilege)
	}

	parameters := make(map[string]map[string]any)
	for _, raw := range arrayValue(t, operation, "parameters") {
		parameter := resolveReference(t, document, raw).(map[string]any)
		parameters[stringValue(parameter, "name")] = parameter
	}
	for _, name := range []string{"dryRun", "prune"} {
		parameter, found := parameters[name]
		if !found {
			t.Errorf("provisioning operation is missing %s query parameter", name)
			continue
		}
		schema := objectValue(t, parameter, "schema")
		if defaultValue, ok := schema["default"].(bool); !ok || defaultValue {
			t.Errorf("provisioning %s default = %v, want false", name, schema["default"])
		}
	}

	responses := objectValue(t, operation, "responses")
	for _, status := range []string{"200", "400", "401", "403", "415", "default"} {
		if _, found := responses[status]; !found {
			t.Errorf("provisioning operation is missing %s response", status)
		}
	}
	success := resolveReference(t, document, responses["200"]).(map[string]any)
	content := objectValue(t, success, "content")
	mediaType := objectValue(t, content, "application/json")
	report := resolveReference(t, document, objectValue(t, mediaType, "schema")).(map[string]any)
	required := stringSlice(t, report["required"])
	for _, property := range []string{"dryRun", "prune", "results"} {
		if !slicesContain(required, property) {
			t.Errorf("ProvisioningReport does not require %q", property)
		}
	}

	requestBody := resolveReference(t, document, operation["requestBody"]).(map[string]any)
	requestContent := objectValue(t, requestBody, "content")
	wantMediaTypes := []string{"application/json", "application/yaml"}
	if got := sortedObjectKeys(requestContent); fmt.Sprint(got) != fmt.Sprint(wantMediaTypes) {
		t.Errorf("provisioning request media types = %v, want %v", got, wantMediaTypes)
	}

	wantSpecFields := map[string][]string{
		"blobStore":      {"attributes", "configurationRef", "driver"},
		"repository":     {"allowOverwrite", "blobStore", "endpoints", "format", "formatConfig", "members", "secret", "secretRef", "type", "upstream"},
		"role":           {"description", "privileges"},
		"user":           {"admin", "password", "roles", "secret", "secretRef"},
		"oidcProvider":   {"allowPasswordGrant", "clientId", "clientSecret", "defaultRoles", "groupRoles", "groupsClaim", "issuer", "scopes", "secret", "secretRef"},
		"cleanupPolicy":  {"action", "criteria", "enabled", "keepLast", "order", "repositories"},
		"classification": {"rules"},
		"trustPolicy": {
			"allowedIdentities", "certificateAuthorities", "deniedFingerprints",
			"mode", "publicKeys",
		},
		"downloadGate": {"criteria", "enabled"},
		"webhook":      {"enabled", "events", "repositories", "secret", "secretRef", "url"},
	}
	wantWriteOnlySecrets := map[string][]string{
		"repository":   {"upstream", "secret"},
		"user":         {"password", "secret"},
		"oidcProvider": {"clientSecret", "secret"},
		"webhook":      {"secret"},
	}
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	resourceUnion := objectValue(t, schemas, "ProvisioningResource")
	unionReferences := make(map[string]struct{})
	for _, raw := range arrayValue(t, resourceUnion, "oneOf") {
		unionReferences[stringValue(raw.(map[string]any), "$ref")] = struct{}{}
	}
	if len(unionReferences) != len(wantSpecFields) {
		t.Errorf("provisioning resource union has %d variants, want %d", len(unionReferences), len(wantSpecFields))
	}
	discriminator := objectValue(t, resourceUnion, "discriminator")
	if stringValue(discriminator, "propertyName") != "kind" {
		t.Error("ProvisioningResource discriminator must use kind")
	}
	mapping := objectValue(t, discriminator, "mapping")
	if len(mapping) != len(wantSpecFields) {
		t.Fatalf("provisioning kind mappings = %d, want %d", len(mapping), len(wantSpecFields))
	}
	for kind, fields := range wantSpecFields {
		rawReference, found := mapping[kind]
		if !found {
			t.Errorf("provisioning discriminator is missing kind %q", kind)
			continue
		}
		if _, found := unionReferences[rawReference.(string)]; !found {
			t.Errorf("provisioning kind %q maps outside its oneOf union", kind)
		}
		resourceSchema := resolveReference(
			t,
			document,
			map[string]any{"$ref": rawReference},
		).(map[string]any)
		if additional, ok := resourceSchema["additionalProperties"].(bool); !ok || additional {
			t.Errorf("provisioning %s resource schema is not closed", kind)
		}
		resourceProperties := objectValue(t, resourceSchema, "properties")
		kindSchema := objectValue(t, resourceProperties, "kind")
		if got := stringSlice(t, kindSchema["enum"]); len(got) != 1 || got[0] != kind {
			t.Errorf("provisioning resource kind enum = %v, want [%s]", got, kind)
		}
		if slicesContain(stringSlice(t, resourceSchema["required"]), "spec") {
			t.Errorf("provisioning %s spec is required despite partial runtime semantics", kind)
		}
		specProperty := objectValue(t, resourceProperties, "spec")
		if nullable, _ := specProperty["nullable"].(bool); !nullable {
			t.Errorf("provisioning %s spec does not document null as an empty overlay", kind)
		}
		specParts := arrayValue(t, specProperty, "allOf")
		if len(specParts) != 1 {
			t.Fatalf("provisioning %s spec has %d allOf entries, want 1", kind, len(specParts))
		}
		specSchema := resolveReference(t, document, specParts[0]).(map[string]any)
		if additional, ok := specSchema["additionalProperties"].(bool); !ok || additional {
			t.Errorf("provisioning %s spec schema is not closed", kind)
		}
		specProperties := objectValue(t, specSchema, "properties")
		gotFields := sortedObjectKeys(specProperties)
		sort.Strings(fields)
		if fmt.Sprint(gotFields) != fmt.Sprint(fields) {
			t.Errorf("provisioning %s spec fields = %v, want %v", kind, gotFields, fields)
		}
		for _, forbidden := range []string{"name", "createdAt", "updatedAt"} {
			if slicesContain(gotFields, forbidden) {
				t.Errorf("provisioning %s spec exposes ignored field %q", kind, forbidden)
			}
		}
		for _, secretField := range wantWriteOnlySecrets[kind] {
			secretSchema := objectValue(t, specProperties, secretField)
			if writeOnly, _ := secretSchema["writeOnly"].(bool); !writeOnly {
				t.Errorf("provisioning %s.%s is not writeOnly", kind, secretField)
			}
		}
	}
}

func assertProvisioningOwnershipContract(t *testing.T, document map[string]any) {
	t.Helper()
	paths := objectValue(t, document, "paths")
	provision := objectValue(t, paths, "/api/v1/provision")
	list := objectValue(t, provision, "get")
	if privilege := stringValue(list, "x-suxen-required-privilege"); privilege != "admin:provision:read" {
		t.Errorf("managed-resource listing privilege = %q, want admin:provision:read", privilege)
	}

	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	for _, schemaName := range []string{
		"BlobStore",
		"Repository",
		"User",
		"Role",
		"OIDCProvider",
		"CleanupPolicy",
		"ClassificationConfig",
		"DownloadGate",
		"TrustPolicy",
		"Webhook",
	} {
		schema := objectValue(t, schemas, schemaName)
		if !schemaHasRequiredReadOnlyProperty(t, document, schema, "managed") {
			t.Errorf("%s does not require a read-only managed property", schemaName)
		}
	}

	wantForce := map[string]struct{}{
		"updateBlobStore": {}, "deleteBlobStore": {},
		"updateRepository": {}, "deleteRepository": {},
		"setClassification": {}, "deleteClassification": {},
		"setDownloadGate": {}, "deleteDownloadGate": {},
		"setTrustPolicy": {}, "deleteTrustPolicy": {},
		"updateUser": {}, "deleteUser": {}, "setUserRoles": {},
		"updateRole": {}, "deleteRole": {},
		"updateOIDCProvider": {}, "deleteOIDCProvider": {},
		"updateCleanupPolicy": {}, "deleteCleanupPolicy": {},
		"putWebhook": {}, "deleteWebhook": {},
	}
	for routePath, rawPathItem := range paths {
		pathItem := rawPathItem.(map[string]any)
		for method, rawOperation := range pathItem {
			if _, isMethod := openAPIMethods[method]; !isMethod {
				continue
			}
			operation := rawOperation.(map[string]any)
			operationID := stringValue(operation, "operationId")
			_, shouldForce := wantForce[operationID]
			hasForce := operationHasParameterReference(
				t,
				operation,
				"#/components/parameters/Force",
			)
			if hasForce != shouldForce {
				t.Errorf("%s %s force parameter = %v, want %v", strings.ToUpper(method), routePath, hasForce, shouldForce)
			}
			if shouldForce {
				responses := objectValue(t, operation, "responses")
				if _, found := responses["400"]; !found {
					t.Errorf(
						"%s %s does not document invalid force values",
						strings.ToUpper(method),
						routePath,
					)
				}
				if _, found := responses["409"]; !found {
					t.Errorf("%s %s does not document managed-resource conflicts", strings.ToUpper(method), routePath)
				}
			}
		}
	}
}

func schemaHasRequiredReadOnlyProperty(
	t *testing.T,
	document map[string]any,
	schema map[string]any,
	propertyName string,
) bool {
	t.Helper()
	resolved := resolveReference(t, document, schema).(map[string]any)
	properties, _ := resolved["properties"].(map[string]any)
	if property, found := properties[propertyName]; found {
		propertySchema := resolveReference(t, document, property).(map[string]any)
		readOnly, _ := propertySchema["readOnly"].(bool)
		return readOnly && slicesContain(stringSlice(t, resolved["required"]), propertyName)
	}
	parts, _ := resolved["allOf"].([]any)
	for _, rawPart := range parts {
		part := resolveReference(t, document, rawPart).(map[string]any)
		if schemaHasRequiredReadOnlyProperty(t, document, part, propertyName) {
			return true
		}
	}
	return false
}

func operationHasParameterReference(
	t *testing.T,
	operation map[string]any,
	reference string,
) bool {
	t.Helper()
	parameters, _ := operation["parameters"].([]any)
	for _, rawParameter := range parameters {
		parameter := rawParameter.(map[string]any)
		if stringValue(parameter, "$ref") == reference {
			return true
		}
	}
	return false
}

func assertClosedRequestSchema(
	t *testing.T,
	document map[string]any,
	rawSchema any,
	operationKey string,
) {
	t.Helper()
	schema := resolveReference(t, document, rawSchema).(map[string]any)
	for _, composition := range []string{"allOf", "oneOf"} {
		rawParts, found := schema[composition].([]any)
		if !found {
			continue
		}
		for _, part := range rawParts {
			assertClosedRequestSchema(t, document, part, operationKey)
		}
	}
	if stringValue(schema, "type") == "array" {
		if items, found := schema["items"]; found {
			assertClosedRequestSchema(t, document, items, operationKey)
		}
		return
	}
	if stringValue(schema, "type") != "object" {
		return
	}
	properties, hasProperties := schema["properties"].(map[string]any)
	if !hasProperties {
		return
	}
	additionalProperties, explicitlyClosed := schema["additionalProperties"].(bool)
	runtimeMap, _ := schema["x-suxen-runtime-map"].(bool)
	if !runtimeMap && (!explicitlyClosed || additionalProperties) {
		t.Errorf("%s request object accepts fields rejected by the runtime decoder", operationKey)
	}
	for _, property := range properties {
		assertClosedRequestSchema(t, document, property, operationKey)
	}
}

func assertAssetContract(t *testing.T, document map[string]any) {
	t.Helper()
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	asset := objectValue(t, schemas, "Asset")
	properties := objectValue(t, asset, "properties")
	if _, found := properties["classification"]; found {
		t.Error("Asset exposes classification outside its projected attributes")
	}
	if _, found := properties["attributes"]; !found {
		t.Error("Asset must expose projected attributes")
	}
}

func assertProblemDetailsContract(t *testing.T, document map[string]any) {
	t.Helper()
	components := objectValue(t, document, "components")
	schemas := objectValue(t, components, "schemas")
	problem := objectValue(t, schemas, "Problem")
	wantRequired := []string{"code", "detail", "status", "title", "type"}
	gotRequired := stringSlice(t, problem["required"])
	sort.Strings(gotRequired)
	if fmt.Sprint(gotRequired) != fmt.Sprint(wantRequired) {
		t.Errorf("Problem required fields = %v, want %v", gotRequired, wantRequired)
	}
	properties := objectValue(t, problem, "properties")
	if stringValue(objectValue(t, properties, "type"), "format") != "uri" {
		t.Error("Problem.type must be documented as a URI")
	}

	responses := objectValue(t, components, "responses")
	for _, name := range []string{
		"BadRequest",
		"Unauthorized",
		"Forbidden",
		"NotFound",
		"Conflict",
		"UnsupportedMediaType",
		"Error",
	} {
		response := objectValue(t, responses, name)
		content := objectValue(t, response, "content")
		if len(content) != 1 {
			t.Errorf("%s response documents unexpected media types: %v", name, sortedObjectKeys(content))
		}
		mediaType := objectValue(t, content, "application/problem+json")
		schema := objectValue(t, mediaType, "schema")
		if stringValue(schema, "$ref") != "#/components/schemas/Problem" {
			t.Errorf("%s does not use the Problem schema", name)
		}
	}

	description := stringValue(objectValue(t, document, "info"), "description")
	if !strings.Contains(description, "OCI") || !strings.Contains(description, "error envelope") {
		t.Error("OpenAPI info must distinguish the OCI data-plane error envelope")
	}
}

func assertBlobStoreContract(t *testing.T, document map[string]any) {
	t.Helper()
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	blobStore := objectValue(t, schemas, "BlobStore")
	properties := objectValue(t, blobStore, "properties")
	configuration := resolveReference(t, document, properties["configurationRef"]).(map[string]any)
	if len(arrayValue(t, configuration, "oneOf")) != 2 {
		t.Error("ConfigurationReference must select exactly one of env or file")
	}
	if _, exposed := properties["physicalIdentity"]; exposed {
		t.Error("BlobStore schema exposes its physical identity fingerprint")
	}
	for _, schemaName := range []string{
		"BlobStore",
		"BlobStoreCreateRequest",
		"BlobStoreUpdateRequest",
		"ProvisioningBlobStoreSpec",
	} {
		schema := objectValue(t, schemas, schemaName)
		if _, found := objectValue(t, schema, "properties")["attributes"]; !found {
			t.Errorf("%s does not expose blob-store attributes", schemaName)
		}
	}
	uploadPolicy := objectValue(t, schemas, "BlobStoreUploadSessionPolicy")
	if additional, ok := uploadPolicy["additionalProperties"].(bool); !ok || additional {
		t.Error("BlobStoreUploadSessionPolicy must reject unknown fields")
	}

	repository := objectValue(t, schemas, "Repository")
	repositoryProperties := objectValue(t, repository, "properties")
	blobStoreReference := objectValue(t, repositoryProperties, "blobStore")
	if stringValue(blobStoreReference, "$ref") != "#/components/schemas/ResourceName" {
		t.Error("Repository.blobStore must reference a named blob-store resource")
	}
}

func assertCollectionPaginationContract(t *testing.T, document map[string]any) {
	t.Helper()
	wantPages := map[string]string{
		"/api/v1/provision":           "ManagedResourcePage",
		"/api/v1/blob-stores":         "BlobStorePage",
		"/api/v1/repositories":        "RepositoryPage",
		"/api/v1/users":               "UserPage",
		"/api/v1/users/{name}/tokens": "APITokenPage",
		"/api/v1/roles":               "RolePage",
		"/api/v1/oidc-providers":      "OIDCProviderPage",
		"/api/v1/cleanup-policies":    "CleanupPolicyPage",
		"/api/v1/webhooks":            "WebhookPage",
	}
	paths := objectValue(t, document, "paths")
	for path, pageName := range wantPages {
		pathItem := objectValue(t, paths, path)
		operation := objectValue(t, pathItem, "get")
		parameterRefs := make(map[string]struct{})
		for _, raw := range arrayValue(t, operation, "parameters") {
			parameter := raw.(map[string]any)
			parameterRefs[stringValue(parameter, "$ref")] = struct{}{}
		}
		for _, name := range []string{"Limit", "Page", "Cursor"} {
			reference := "#/components/parameters/" + name
			if _, found := parameterRefs[reference]; !found {
				t.Errorf("GET %s is missing %s", path, reference)
			}
		}
		responses := objectValue(t, operation, "responses")
		for _, status := range []string{"400", "409"} {
			if _, found := responses[status]; !found {
				t.Errorf("GET %s is missing documented %s response", path, status)
			}
		}
		success := objectValue(t, responses, "200")
		content := objectValue(t, success, "content")
		mediaType := objectValue(t, content, "application/json")
		schema := objectValue(t, mediaType, "schema")
		wantReference := "#/components/schemas/" + pageName
		if stringValue(schema, "$ref") != wantReference {
			t.Errorf("GET %s response schema = %q, want %q", path, stringValue(schema, "$ref"), wantReference)
		}
		page := resolveReference(t, document, schema).(map[string]any)
		pageProperties := objectValue(t, page, "properties")
		if _, found := pageProperties["items"]; !found {
			t.Errorf("%s has no items property", pageName)
		}
		if _, found := pageProperties["nextCursor"]; !found {
			t.Errorf("%s has no nextCursor property", pageName)
		}
	}

	for path, pageName := range map[string]string{
		"/api/v1/tasks":                      "TaskPage",
		"/api/v1/webhooks/{name}/deliveries": "WebhookDeliveryPage",
	} {
		operation := objectValue(t, objectValue(t, paths, path), "get")
		parameterRefs := make(map[string]struct{})
		for _, raw := range arrayValue(t, operation, "parameters") {
			parameter := raw.(map[string]any)
			parameterRefs[stringValue(parameter, "$ref")] = struct{}{}
		}
		for _, name := range []string{"Limit", "Cursor"} {
			if _, found := parameterRefs["#/components/parameters/"+name]; !found {
				t.Errorf("GET %s is missing %s", path, name)
			}
		}
		if _, found := parameterRefs["#/components/parameters/Page"]; found {
			t.Errorf("GET %s must not offer offset pages for id-based history", path)
		}
		responses := objectValue(t, operation, "responses")
		for _, status := range []string{"400", "409"} {
			if _, found := responses[status]; !found {
				t.Errorf("GET %s is missing documented %s response", path, status)
			}
		}
		success := objectValue(t, responses, "200")
		content := objectValue(t, success, "content")
		mediaType := objectValue(t, content, "application/json")
		schema := objectValue(t, mediaType, "schema")
		if got := stringValue(schema, "$ref"); got != "#/components/schemas/"+pageName {
			t.Errorf("GET %s response schema = %q, want %s", path, got, pageName)
		}
		page := resolveReference(t, document, schema).(map[string]any)
		pageProperties := objectValue(t, page, "properties")
		for _, field := range []string{"items", "nextCursor"} {
			if _, found := pageProperties[field]; !found {
				t.Errorf("%s has no %s property", pageName, field)
			}
		}
		if _, found := pageProperties["total"]; found {
			t.Errorf("%s must not report total for id-based history", pageName)
		}
	}

	parameters := objectValue(t, objectValue(t, document, "components"), "parameters")
	pageParameter := objectValue(t, parameters, "Page")
	if stringValue(pageParameter, "name") != "page" || stringValue(pageParameter, "in") != "query" {
		t.Error("Page must define the page query parameter")
	}
	if !strings.Contains(stringValue(pageParameter, "description"), "Cannot be combined with cursor") {
		t.Error("Page must document mutual exclusion with cursor")
	}
	pageSchema := objectValue(t, pageParameter, "schema")
	if stringValue(pageSchema, "type") != "integer" || pageSchema["minimum"] != float64(1) {
		t.Error("Page must define a positive integer")
	}
	limit := objectValue(t, objectValue(t, parameters, "Limit"), "schema")
	if maximum, _ := limit["maximum"].(float64); maximum != httpx.MaximumCollectionLimit {
		t.Errorf("Limit maximum = %v, want %d", limit["maximum"], httpx.MaximumCollectionLimit)
	}
	cursor := objectValue(t, objectValue(t, parameters, "Cursor"), "schema")
	if maximum, _ := cursor["maxLength"].(float64); maximum != httpx.MaximumCursorLength {
		t.Errorf("Cursor maxLength = %v, want %d", cursor["maxLength"], httpx.MaximumCursorLength)
	}
}

func assertResponseFieldContract(t *testing.T, document map[string]any) {
	t.Helper()
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	tokenCreated := objectValue(t, schemas, "TokenCreated")
	tokenProperties := objectValue(t, tokenCreated, "properties")
	token := objectValue(t, tokenProperties, "token")
	if readOnly, _ := token["readOnly"].(bool); !readOnly {
		t.Error("TokenCreated.token must be documented as one-time response data")
	}
	if writeOnly, _ := token["writeOnly"].(bool); writeOnly {
		t.Error("TokenCreated.token cannot be write-only because the server returns it")
	}

	oidcProvider := objectValue(t, schemas, "OIDCProvider")
	variants := arrayValue(t, oidcProvider, "allOf")
	if len(variants) != 2 {
		t.Fatalf("OIDCProvider allOf variants = %d, want 2", len(variants))
	}
	responseFields := stringSlice(t, variants[1].(map[string]any)["required"])
	for _, name := range []string{"name", "issuer", "clientId", "scopes", "groupsClaim", "createdAt"} {
		if !slicesContain(responseFields, name) {
			t.Errorf("OIDCProvider response does not require %q", name)
		}
	}
}

func assertNamedWebhookContract(t *testing.T, document map[string]any) {
	t.Helper()
	paths := objectValue(t, document, "paths")
	collection := objectValue(t, paths, "/api/v1/webhooks")
	create := objectValue(t, collection, "post")
	createResponses := objectValue(t, create, "responses")
	if _, found := createResponses["404"]; !found {
		t.Error("webhook creation does not document missing repository filters")
	}

	path := objectValue(t, paths, "/api/v1/webhooks/{name}")
	operation := objectValue(t, path, "put")
	requestBody := resolveReference(t, document, operation["requestBody"]).(map[string]any)
	content := objectValue(t, requestBody, "content")
	mediaType := objectValue(t, content, "application/json")
	schema := resolveReference(t, document, objectValue(t, mediaType, "schema")).(map[string]any)
	required := stringSlice(t, schema["required"])
	if slicesContain(required, "name") || slicesContain(required, "secret") {
		t.Errorf("webhook PUT incorrectly requires body name or secret: %v", required)
	}

	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	webhook := objectValue(t, schemas, "Webhook")
	webhookProperties := objectValue(t, webhook, "properties")
	if _, found := webhookProperties["id"]; found {
		t.Error("Webhook exposes a numeric ID in addition to its name identity")
	}
	delivery := objectValue(t, schemas, "WebhookDelivery")
	deliveryProperties := objectValue(t, delivery, "properties")
	if _, found := deliveryProperties["webhookId"]; found {
		t.Error("WebhookDelivery exposes a removed numeric webhook ID")
	}
	if _, found := deliveryProperties["webhookName"]; !found {
		t.Error("WebhookDelivery must identify its webhook by name")
	}
	status := objectValue(t, deliveryProperties, "status")
	wantStatuses := []string{"queued", "delivering", "retry", "delivered", "dead"}
	if got := stringSlice(t, status["enum"]); fmt.Sprint(got) != fmt.Sprint(wantStatuses) {
		t.Errorf("WebhookDelivery statuses = %v, want %v", got, wantStatuses)
	}
}

func assertPathAuthoritativeRequestContracts(t *testing.T, document map[string]any) {
	t.Helper()
	testCases := []struct {
		method       string
		path         string
		nameRequired bool
	}{
		{method: "post", path: "/api/v1/blob-stores", nameRequired: true},
		{method: "put", path: "/api/v1/blob-stores/{name}"},
		{method: "post", path: "/api/v1/repositories", nameRequired: true},
		{method: "put", path: "/api/v1/repositories/{name}"},
		{method: "post", path: "/api/v1/roles", nameRequired: true},
		{method: "put", path: "/api/v1/roles/{name}"},
		{method: "post", path: "/api/v1/oidc-providers", nameRequired: true},
		{method: "put", path: "/api/v1/oidc-providers/{name}"},
		{method: "post", path: "/api/v1/cleanup-policies", nameRequired: true},
		{method: "put", path: "/api/v1/cleanup-policies/{name}"},
	}

	paths := objectValue(t, document, "paths")
	for _, testCase := range testCases {
		path := objectValue(t, paths, testCase.path)
		operation := objectValue(t, path, testCase.method)
		requestBody := resolveReference(t, document, operation["requestBody"]).(map[string]any)
		content := objectValue(t, requestBody, "content")
		mediaType := objectValue(t, content, "application/json")
		schema := resolveReference(t, document, mediaType["schema"]).(map[string]any)
		var required []string
		if rawRequired, found := schema["required"]; found {
			required = stringSlice(t, rawRequired)
		}
		if slicesContain(required, "name") != testCase.nameRequired {
			t.Errorf(
				"%s %s name requirement = %t, want %t",
				strings.ToUpper(testCase.method),
				testCase.path,
				slicesContain(required, "name"),
				testCase.nameRequired,
			)
		}
	}
}

func assertStablePredicateSchema(t *testing.T, document map[string]any) {
	t.Helper()
	schemas := objectValue(t, objectValue(t, document, "components"), "schemas")
	predicate := objectValue(t, schemas, "Predicate")
	properties := objectValue(t, predicate, "properties")
	operator := objectValue(t, properties, "op")
	gotOperators := stringSlice(t, operator["enum"])
	wantOperators := []string{
		"=",
		"!=",
		"<",
		"<=",
		">",
		">=",
		"before",
		"after",
		"matches",
		"contains",
		"in",
		"not-in",
		"exists",
		"absent",
	}
	if fmt.Sprint(gotOperators) != fmt.Sprint(wantOperators) {
		t.Errorf("cleanup predicate operators = %v, want %v", gotOperators, wantOperators)
	}

	reserved := objectValue(t, predicate, "x-suxen-reserved-paths")
	known := assetattrs.KnownAttributePaths()
	slices.Sort(known)
	documented := make([]string, 0, len(known))
	for root, fields := range reserved {
		if !assetattrs.IsReservedNamespace(root) {
			t.Errorf("OpenAPI documents unreserved root %q", root)
		}
		for _, field := range stringSlice(t, fields) {
			if root == assetattrs.ClassificationNamespace && field == "<key>" {
				continue // Classification label names are assigned by policy rules.
			}
			documented = append(documented, root+"."+field)
		}
	}
	slices.Sort(documented)
	if !slices.Equal(documented, known) {
		t.Errorf("OpenAPI stable predicate paths = %v, want discovery catalogue %v", documented, known)
	}
	if !slices.Equal(stringSlice(t, reserved[assetattrs.ClassificationNamespace]), []string{"<key>"}) {
		t.Error("OpenAPI must describe classification keys as dynamic")
	}
	description := stringValue(predicate, "description")
	if !strings.Contains(description, "every registered format name returned in discovery.formats") ||
		!strings.Contains(description, "Other roots address writer-owned attribute namespaces") {
		t.Error("OpenAPI must describe format namespaces as dynamically reserved and other roots as writer-owned")
	}
	for _, formatName := range spiformat.Names() {
		if !assetattrs.IsReservedNamespace(formatName) || !slices.Contains(assetattrs.ReservedRoots(), formatName) {
			t.Errorf("registered format %q is missing from reserved namespace catalogue", formatName)
		}
	}
	criteria := objectValue(t, schemas, "Criteria")
	if stringValue(criteria, "type") != "array" {
		t.Fatal("Criteria must remain an ordered predicate array")
	}
}

func assertAllLocalReferencesResolve(t *testing.T, document map[string]any) {
	t.Helper()
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if _, found := typed["$ref"]; found {
				resolveReference(t, document, typed)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
}

func resolveReference(t *testing.T, document map[string]any, raw any) any {
	t.Helper()
	object, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("reference target is not an object: %T", raw)
	}
	reference, found := object["$ref"].(string)
	if !found {
		return object
	}
	if !strings.HasPrefix(reference, "#/") {
		t.Fatalf("external reference %q is not supported by the embedded contract", reference)
	}
	var current any = document
	for _, token := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		container, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("reference %q traverses a non-object", reference)
		}
		current, found = container[token]
		if !found {
			t.Fatalf("reference %q does not resolve", reference)
		}
	}
	return current
}

func hasSuccessResponse(responses map[string]any) bool {
	for status := range responses {
		if len(status) == 3 && status[0] == '2' {
			return true
		}
	}
	return false
}

func placeholderNames(path string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, part := range strings.Split(path, "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			result[strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")] = struct{}{}
		}
	}
	return result
}

func representativeRoutePath(path string) string {
	path = strings.ReplaceAll(path, "{name}", "example")
	path = strings.ReplaceAll(path, "{id}", "42")
	return strings.ReplaceAll(path, "{namespace}", "scanner")
}

func objectValue(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object", key)
	}
	return value
}

func arrayValue(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	value, ok := object[key].([]any)
	if !ok {
		t.Fatalf("%q is not an array", key)
	}
	return value
}

func stringValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func stringSlice(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value is not an array: %T", value)
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("array item is not a string: %T", item)
		}
		result = append(result, text)
	}
	return result
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortedObjectKeys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestAssetListingOpenAPIUsesKeysetCursor(t *testing.T) {
	document := decodeOpenAPITemplate(t)
	paths := objectValue(t, document, "paths")
	path := objectValue(t, paths, "/api/v1/repositories/{name}/assets")
	operation := objectValue(t, path, "get")
	refs := make(map[string]bool)
	for _, raw := range arrayValue(t, operation, "parameters") {
		parameter := raw.(map[string]any)
		refs[stringValue(parameter, "$ref")] = true
	}
	for _, name := range []string{"Limit", "Cursor"} {
		if !refs["#/components/parameters/"+name] {
			t.Errorf("asset listing is missing %s", name)
		}
	}
	if refs["#/components/parameters/Page"] {
		t.Error("asset listing must not expose unbounded offset paging")
	}
	responses := objectValue(t, operation, "responses")
	success := objectValue(t, responses, "200")
	content := objectValue(t, success, "content")
	mediaType := objectValue(t, content, "application/json")
	schema := objectValue(t, mediaType, "schema")
	if stringValue(schema, "$ref") != "#/components/schemas/AssetPage" {
		t.Error("asset listing must return AssetPage")
	}
	assetPage := resolveReference(t, document, schema).(map[string]any)
	properties := objectValue(t, assetPage, "properties")
	if _, found := properties["total"]; found {
		t.Error("asset listing must not promise an unbounded total count")
	}
}

func TestDiscoveryOpenAPIUsesBoundedCursorPages(t *testing.T) {
	document := decodeOpenAPITemplate(t)
	paths := objectValue(t, document, "paths")
	for path, pageName := range map[string]string{
		"/api/v1/browse":                     "BrowseRepositoryPage",
		"/api/v1/search":                     "RepositoryBrowsePage",
		"/api/v1/repositories/{name}/browse": "RepositoryBrowsePage",
	} {
		operation := objectValue(t, objectValue(t, paths, path), "get")
		refs := make(map[string]bool)
		for _, raw := range arrayValue(t, operation, "parameters") {
			parameter := raw.(map[string]any)
			refs[stringValue(parameter, "$ref")] = true
		}
		for _, name := range []string{"Limit", "Cursor"} {
			if !refs["#/components/parameters/"+name] {
				t.Errorf("GET %s is missing %s", path, name)
			}
		}
		if refs["#/components/parameters/Page"] {
			t.Errorf("GET %s must not expose offset pages", path)
		}
		responses := objectValue(t, operation, "responses")
		success := objectValue(t, responses, "200")
		content := objectValue(t, success, "content")
		mediaType := objectValue(t, content, "application/json")
		schema := objectValue(t, mediaType, "schema")
		if stringValue(schema, "$ref") != "#/components/schemas/"+pageName {
			t.Errorf("GET %s must return %s", path, pageName)
		}
		page := resolveReference(t, document, schema).(map[string]any)
		if _, found := objectValue(t, page, "properties")["total"]; found {
			t.Errorf("GET %s must not promise an unbounded total", path)
		}
	}
}
