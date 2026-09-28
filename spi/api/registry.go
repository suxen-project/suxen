package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/suxen-project/suxen/spi/internal/ident"
)

var registry = struct {
	sync.RWMutex
	plugins map[string][]Route
}{
	plugins: make(map[string][]Route),
}

// Register mounts routes at /api/v1/plugins/{pluginID}/. It panics for invalid or
// duplicate registrations because those are build errors, not runtime
// conditions.
func Register(pluginID string, routes ...Route) {
	if !ident.Valid(pluginID) {
		panic("api: register requires a lowercase plugin identifier")
	}
	if len(routes) == 0 {
		panic("api: register requires at least one route")
	}

	seen := make(map[string]struct{}, len(routes))
	frozenRoutes := make([]Route, 0, len(routes))
	for _, route := range routes {
		path := route.Path
		if !validRoutePath(path) {
			panic("api: route path " + route.Path + " is invalid")
		}
		if len(route.Methods) == 0 {
			panic("api: route " + pluginID + "/" + path + " requires methods")
		}
		if route.Handle == nil {
			panic("api: route " + pluginID + "/" + path + " requires a handler")
		}
		methods := make(map[string]struct{}, len(route.Methods))
		for _, method := range route.Methods {
			if !validMethod(method) {
				panic("api: route " + pluginID + "/" + path + " has invalid method " + method)
			}
			if _, duplicate := methods[method]; duplicate {
				panic("api: route " + pluginID + "/" + path + " has duplicate method " + method)
			}
			methods[method] = struct{}{}
			operation, documented := route.Operations[method]
			if !documented || !hasSuccessResponse(operation) {
				panic("api: route " + pluginID + "/" + path + " requires an explicit valid success response for " + method)
			}
			if body, set := operation["requestBody"]; set && !validBody(body) {
				panic("api: route " + pluginID + "/" + path + " requires request body schemas for " + method)
			}
		}
		for method := range route.Operations {
			if _, served := methods[method]; !served {
				panic("api: route " + pluginID + "/" + path + " documents " + method + " but does not serve it")
			}
		}
		// Two patterns that differ only in parameter names would shadow each
		// other, so duplicates are detected on the shape, not the text.
		shape := routeShape(path)
		if _, exists := seen[shape]; exists {
			panic("api: register duplicate path /api/v1/plugins/" + pluginID + "/" + path)
		}
		seen[shape] = struct{}{}
		route.Methods = append([]string(nil), route.Methods...)
		// The served document must not change when a plugin reuses or mutates
		// the maps it supplied during registration.
		frozen, err := copyOperations(route.Operations)
		if err != nil {
			panic(fmt.Sprintf("api: route %s/%s has invalid operations: %v", pluginID, path, err))
		}
		route.Operations = frozen
		frozenRoutes = append(frozenRoutes, route)
	}
	// Static segments win over parameters regardless of registration order,
	// so "widgets/all" is matched before "widgets/{name}".
	sort.SliceStable(frozenRoutes, func(i, j int) bool {
		return moreSpecific(splitPattern(frozenRoutes[i].Path), splitPattern(frozenRoutes[j].Path))
	})

	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.plugins[pluginID]; exists {
		panic("api: register duplicate plugin " + pluginID)
	}
	registry.plugins[pluginID] = frozenRoutes
}

// Unregister removes a plugin. Host tests call it from t.Cleanup; plugins
// must not.
func Unregister(pluginID string) {
	registry.Lock()
	delete(registry.plugins, pluginID)
	registry.Unlock()
}

// Registered reports whether pluginID has control-plane routes in this binary.
func Registered(pluginID string) bool {
	registry.RLock()
	defer registry.RUnlock()
	_, found := registry.plugins[pluginID]
	return found
}

// Routes returns independent copies of the registered routes in match order.
// Changing the returned methods or operations does not change the registry.
// The result is nil for unknown plugins.
func Routes(pluginID string) []Route {
	registry.RLock()
	defer registry.RUnlock()
	routes, found := registry.plugins[pluginID]
	if !found {
		return nil
	}
	copies := make([]Route, len(routes))
	for index, route := range routes {
		copies[index] = copyRoute(route)
	}
	return copies
}

// Names returns the sorted registered plugin identifiers.
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	names := make([]string, 0, len(registry.plugins))
	for name := range registry.plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Match finds a registered plugin route for the path segments after /api/v1.
// parts must start with "plugins", followed by the plugin identifier. The
// returned route is independent of the registry and may be changed by callers.
func Match(parts []string) (pluginID string, route Route, parameters map[string]string, ok bool) {
	if len(parts) < 2 || parts[0] != "plugins" {
		return "", Route{}, nil, false
	}
	pluginID = parts[1]
	registry.RLock()
	defer registry.RUnlock()
	routes, found := registry.plugins[pluginID]
	if !found {
		return "", Route{}, nil, false
	}
	remainder := parts[2:]
	for _, candidate := range routes {
		parameters, matches := matchPath(candidate.Path, remainder)
		if matches {
			return pluginID, copyRoute(candidate), parameters, true
		}
	}
	return pluginID, Route{}, nil, false
}

// validRoutePath accepts only unreserved URL path segments and whole-segment
// parameters. The empty path names the plugin root; "/" is not an alias for it.
func validRoutePath(path string) bool {
	if path == "" {
		return true
	}
	parameters := make(map[string]struct{})
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := part[1 : len(part)-1]
			if !ident.Valid(name) {
				return false
			}
			if _, duplicate := parameters[name]; duplicate {
				return false
			}
			parameters[name] = struct{}{}
			continue
		}
		for _, character := range part {
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') ||
				character == '-' || character == '_' || character == '.' || character == '~' {
				continue
			}
			return false
		}
	}
	return true
}

// The host documents routes as OpenAPI operations. CONNECT has no OpenAPI
// path-item operation, and extension methods need an explicit policy first.
func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func copyRoute(route Route) Route {
	route.Methods = append([]string(nil), route.Methods...)
	operations, err := copyOperations(route.Operations)
	if err != nil {
		panic(fmt.Sprintf("api: registered route %s has invalid operations: %v", route.Path, err))
	}
	route.Operations = operations
	return route
}

func copyOperations(operations map[string]Operation) (map[string]Operation, error) {
	encoded, err := json.Marshal(operations)
	if err != nil {
		return nil, err
	}
	var copied map[string]Operation
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	return copied, nil
}

func hasSuccessResponse(operation Operation) bool {
	if operation == nil {
		return false
	}
	responses, ok := operation["responses"].(map[string]any)
	if !ok {
		return false
	}
	for status := range responses {
		code, err := strconv.Atoi(status)
		if err == nil && len(status) == 3 && code >= 200 && code < 300 {
			response, ok := responses[status].(map[string]any)
			if !ok {
				continue
			}
			description, _ := response["description"].(string)
			if description == "" {
				continue
			}
			if content, set := response["content"]; set && !validContent(content) {
				continue
			}
			return true
		}
	}
	return false
}

func validBody(value any) bool {
	body, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return validContent(body["content"])
}

func validContent(value any) bool {
	content, ok := value.(map[string]any)
	if !ok || len(content) == 0 {
		return false
	}
	for _, value := range content {
		mediaType, ok := value.(map[string]any)
		if !ok {
			return false
		}
		schema, ok := mediaType["schema"].(map[string]any)
		if !ok || !validInlineSchema(schema) {
			return false
		}
	}
	return true
}

func validInlineSchema(schema map[string]any) bool {
	return len(schema) > 0 && !containsReference(schema)
}

func containsReference(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, nested := range value {
			if key == "$ref" || containsReference(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range value {
			if containsReference(nested) {
				return true
			}
		}
	}
	return false
}

func matchPath(pattern string, parts []string) (map[string]string, bool) {
	patternParts := splitPattern(pattern)
	if len(patternParts) != len(parts) {
		return nil, false
	}
	parameters := make(map[string]string)
	for index, actual := range parts {
		patternPart := patternParts[index]
		if isPathParameter(patternPart) {
			if actual == "" {
				return nil, false
			}
			parameters[patternPart[1:len(patternPart)-1]] = actual
			continue
		}
		if patternPart != actual {
			return nil, false
		}
	}
	return parameters, true
}

// routeShape replaces every parameter segment with "{}" so patterns compare
// by structure.
func routeShape(pattern string) string {
	parts := splitPattern(pattern)
	for index, part := range parts {
		if isPathParameter(part) {
			parts[index] = "{}"
		}
	}
	return strings.Join(parts, "/")
}

// moreSpecific orders patterns so that, at the first position where one has
// a static segment and the other a parameter, the static one sorts first.
// Patterns of different lengths never compete for a request and are ordered
// by length only to keep the sort total.
func moreSpecific(left, right []string) bool {
	if len(left) != len(right) {
		return len(left) < len(right)
	}
	for index := range left {
		leftParameter := isPathParameter(left[index])
		rightParameter := isPathParameter(right[index])
		if leftParameter != rightParameter {
			return rightParameter
		}
	}
	return false
}

func splitPattern(pattern string) []string {
	if pattern == "" {
		return nil
	}
	return strings.Split(pattern, "/")
}

func isPathParameter(part string) bool {
	return len(part) >= 2 && part[0] == '{' && part[len(part)-1] == '}'
}
