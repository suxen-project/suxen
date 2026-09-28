package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	spiapi "github.com/suxen-project/suxen/spi/api"
)

func TestOpenAPIDocumentsPluginRoutes(t *testing.T) {
	t.Parallel()
	handle := func(spiapi.Context) {}
	spiapi.Register("docplugin", spiapi.Route{
		Path:    "",
		Methods: []string{http.MethodGet},
		Handle:  handle,
		Operations: map[string]spiapi.Operation{
			http.MethodGet: {
				"operationId": "docpluginStatus",
				"summary":     "Plugin status",
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Status",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		},
	}, spiapi.Route{
		Path:    "items/{name}",
		Methods: []string{http.MethodGet, http.MethodDelete},
		Handle:  handle,
		Operations: map[string]spiapi.Operation{
			http.MethodGet:    testPluginOperation(),
			http.MethodDelete: testPluginOperation(),
		},
	})
	t.Cleanup(func() {
		spiapi.Unregister("docplugin")
	})

	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/openapi.json", nil, false)
	assertStatus(t, response, http.StatusOK)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	loader := openapi3.NewLoader()
	validated, err := loader.LoadFromData(body)
	if err != nil {
		t.Fatalf("load served OpenAPI document: %v", err)
	}
	if err := validated.Validate(context.Background()); err != nil {
		t.Fatalf("served document with plugin routes is invalid: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	root := pluginObject(t, document, "paths", "/api/v1/plugins/docplugin", "get")
	if root["operationId"] != "docpluginStatus" || root["summary"] != "Plugin status" {
		t.Fatalf("plugin-supplied operation was not kept: %+v", root)
	}
	if root["x-suxen-required-privilege"] != "admin:plugin-docplugin:read" {
		t.Fatalf("root privilege = %v", root["x-suxen-required-privilege"])
	}
	if _, kept := pluginObject(t, root, "responses", "200")["content"]; !kept {
		t.Fatalf("plugin-supplied success response was replaced: %+v", root["responses"])
	}
	for _, code := range []string{"401", "403", "default"} {
		if _, set := pluginObject(t, root, "responses")[code]; !set {
			t.Fatalf("shared response %s missing: %+v", code, root["responses"])
		}
	}

	item := pluginObject(t, document, "paths", "/api/v1/plugins/docplugin/items/{name}", "delete")
	if item["x-suxen-required-privilege"] != "admin:plugin-docplugin:write" {
		t.Fatalf("delete privilege = %v", item["x-suxen-required-privilege"])
	}
	if item["operationId"] != "plugin_docplugin_delete_items_name" {
		t.Fatalf("generated operationId = %v", item["operationId"])
	}
	parameters, _ := item["parameters"].([]any)
	if len(parameters) != 1 {
		t.Fatalf("path parameters = %+v", item["parameters"])
	}
	parameter, _ := parameters[0].(map[string]any)
	if parameter["name"] != "name" || parameter["in"] != "path" || parameter["required"] != true {
		t.Fatalf("path parameter = %+v", parameter)
	}
	if _, set := pluginObject(t, item, "responses")["204"]; !set {
		t.Fatalf("declared success response missing: %+v", item["responses"])
	}
}

func pluginObject(t *testing.T, value any, keys ...string) map[string]any {
	t.Helper()
	current, ok := value.(map[string]any)
	for _, key := range keys {
		if !ok {
			t.Fatalf("expected an object before %q", key)
		}
		current, ok = current[key].(map[string]any)
	}
	if !ok {
		t.Fatalf("missing object at %v", keys)
	}
	return current
}
