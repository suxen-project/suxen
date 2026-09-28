package server

import (
	"encoding/json"
	"fmt"
	"strings"

	spiapi "github.com/suxen-project/suxen/spi/api"
)

// documentPluginRoutes adds every registered plugin route to the served
// OpenAPI document so the contract a client downloads is complete for the
// binary it talks to. Plugin-supplied operations are copied by value; the
// host then sets the fields that encode its own guarantees — the required
// privilege, the path parameters, and the shared error responses — so a
// plugin cannot document a weaker contract than the one enforced.
func documentPluginRoutes(document map[string]any) error {
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		return fmt.Errorf("document has no paths object")
	}
	for _, pluginID := range spiapi.Names() {
		for _, route := range spiapi.Routes(pluginID) {
			template := "/api/v1/plugins/" + pluginID
			if route.Path != "" {
				template += "/" + route.Path
			}
			if _, exists := paths[template]; exists {
				return fmt.Errorf("plugin route %s collides with a core path", template)
			}
			item := make(map[string]any, len(route.Methods))
			for _, method := range route.Methods {
				operation, err := pluginOperation(pluginID, route, method)
				if err != nil {
					return fmt.Errorf("%s %s: %w", method, template, err)
				}
				item[strings.ToLower(method)] = operation
			}
			paths[template] = item
		}
	}
	return nil
}

func pluginOperation(pluginID string, route spiapi.Route, method string) (map[string]any, error) {
	operation := make(map[string]any)
	supplied := route.Operations[method]
	encoded, err := json.Marshal(supplied)
	if err != nil {
		return nil, fmt.Errorf("encode plugin operation: %w", err)
	}
	if err := json.Unmarshal(encoded, &operation); err != nil {
		return nil, fmt.Errorf("decode plugin operation: %w", err)
	}

	parts := []string{"plugins", pluginID}
	if route.Path != "" {
		parts = append(parts, strings.Split(route.Path, "/")...)
	}
	operation["x-suxen-required-privilege"] = controlPlanePrivilegeRequirement(parts, method)
	if _, set := operation["operationId"]; !set {
		operation["operationId"] = pluginOperationID(pluginID, method, route.Path)
	}
	if _, set := operation["tags"]; !set {
		operation["tags"] = []any{"Plugins"}
	}
	if _, set := operation["summary"]; !set {
		operation["summary"] = fmt.Sprintf(
			"%s plugin: %s /api/v1/plugins/%s",
			pluginID,
			method,
			strings.TrimSuffix(pluginID+"/"+route.Path, "/"),
		)
	}
	if parameters := withPathParameters(operation["parameters"], route.Path); len(parameters) > 0 {
		operation["parameters"] = parameters
	} else {
		delete(operation, "parameters")
	}
	operation["responses"] = withSharedResponses(operation["responses"])
	return operation, nil
}

// pluginOperationID derives a stable, document-unique identifier such as
// plugin_myplugin_get_widgets_name for plugins that do not supply one.
func pluginOperationID(pluginID string, method string, path string) string {
	id := "plugin_" + pluginID + "_" + strings.ToLower(method)
	if path == "" {
		return id
	}
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, path)
	for strings.Contains(sanitized, "__") {
		sanitized = strings.ReplaceAll(sanitized, "__", "_")
	}
	return id + "_" + strings.Trim(sanitized, "_")
}

// withPathParameters declares every {name} segment of the route path that the
// plugin did not declare itself, so the document validates and clients see
// the parameter.
func withPathParameters(existing any, path string) []any {
	parameters, _ := existing.([]any)
	declared := make(map[string]bool)
	for _, parameter := range parameters {
		object, _ := parameter.(map[string]any)
		if object["in"] != "path" {
			continue
		}
		if name, _ := object["name"].(string); name != "" {
			declared[name] = true
		}
	}
	for _, segment := range strings.Split(path, "/") {
		if len(segment) < 2 || segment[0] != '{' || segment[len(segment)-1] != '}' {
			continue
		}
		name := segment[1 : len(segment)-1]
		if declared[name] {
			continue
		}
		declared[name] = true
		parameters = append(parameters, map[string]any{
			"name":     name,
			"in":       "path",
			"required": true,
			"schema":   map[string]any{"type": "string"},
		})
	}
	return parameters
}

// withSharedResponses adds the 401, 403, and default problem responses every
// control-plane operation can return. The plugin must supply a success response.
func withSharedResponses(existing any) map[string]any {
	responses, _ := existing.(map[string]any)
	shared := map[string]string{
		"401":     "Unauthorized",
		"403":     "Forbidden",
		"default": "Error",
	}
	for code, name := range shared {
		if _, set := responses[code]; !set {
			responses[code] = map[string]any{"$ref": "#/components/responses/" + name}
		}
	}
	return responses
}
