// Package api is the public service-provider interface for plugin control-plane
// HTTP routes.
//
// The host mounts every registered plugin under /api/v1/plugins/{pluginID}/.
// This namespace is independent of core control-plane resource names.
package api

import "net/http"

// Route is one HTTP endpoint relative to /api/v1/plugins/{pluginID}/.
type Route struct {
	// Path is a canonical relative path: unreserved URL characters in static
	// segments, with no leading or trailing slash, empty segment, dot segment,
	// or percent escape. Whole "{name}" segments capture parameters; names
	// are unique lowercase identifiers. The empty path names the plugin root
	// at /api/v1/plugins/{pluginID}; "/" is invalid. When several routes
	// match, static segments win regardless of registration order.
	Path string
	// Methods contains distinct, uppercase OpenAPI HTTP methods: GET, HEAD,
	// POST, PUT, PATCH, DELETE, OPTIONS, or TRACE.
	Methods []string
	// Handle serves the request. It must not be nil.
	Handle func(Context)
	// Operations documents every method in the served OpenAPI document.
	// Each method requires an operation with an explicit success response.
	// Request and response bodies must describe their schemas inline. Register
	// copies this data, and Routes and Match return independent copies.
	Operations map[string]Operation
}

// Operation is an OpenAPI 3 operation object for one method of a Route.
// The host owns the path, the x-suxen-required-privilege extension, the
// path parameters it derives from "{name}" segments, and the shared
// 401/403/default error responses; a plugin supplies the rest — summary,
// description, operationId, requestBody, success responses — with schemas
// written inline, since plugins cannot add to the document's components.
type Operation map[string]any

// Context is the per-request handle a plugin route receives. The host has
// already checked admin:plugin-{pluginID}:read or :write and protects
// cookie-authenticated mutations before Handle runs. A caller may be
// anonymous when the anonymous role grants the required privilege.
type Context interface {
	PluginID() string
	Parameters() map[string]string
	Request() *http.Request
	ResponseWriter() http.ResponseWriter
	// DecodeJSON reads the request body into destination under the same size
	// limit core routes apply. On failure it has already written a problem
	// response and returns false, so the handler just returns.
	DecodeJSON(destination any) bool
	WriteJSON(status int, value any)
	WriteProblem(status int, code, message string)
}

// Allows reports whether method is one of the route's registered methods.
func (route Route) Allows(method string) bool {
	for _, allowed := range route.Methods {
		if allowed == method {
			return true
		}
	}
	return false
}
