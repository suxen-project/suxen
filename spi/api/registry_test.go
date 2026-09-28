package api

import (
	"net/http"
	"slices"
	"sync"
	"testing"
)

func registerAPIForTest(t *testing.T, pluginID string, routes ...Route) {
	t.Helper()
	for index := range routes {
		if routes[index].Operations == nil {
			routes[index].Operations = make(map[string]Operation)
		}
		for _, method := range routes[index].Methods {
			if _, set := routes[index].Operations[method]; !set {
				routes[index].Operations[method] = testOperation()
			}
		}
	}
	Register(pluginID, routes...)
	t.Cleanup(func() {
		Unregister(pluginID)
	})
}

func testOperation() Operation {
	return Operation{"responses": map[string]any{"204": map[string]any{"description": "Complete"}}}
}

func TestRegisterAndMatch(t *testing.T) {
	registerAPIForTest(t, "registry-test-plugin", Route{
		Path:    "widgets/{name}",
		Methods: []string{http.MethodGet},
		Handle:  func(Context) {},
	}, Route{
		Path:    "",
		Methods: []string{http.MethodGet},
		Handle:  func(Context) {},
	})
	if !Registered("registry-test-plugin") {
		t.Fatal("registered plugin not reported")
	}
	if !slices.Contains(Names(), "registry-test-plugin") {
		t.Fatalf("Names() = %v", Names())
	}

	pluginID, route, parameters, ok := Match([]string{"plugins", "registry-test-plugin", "widgets", "alpha"})
	if !ok || pluginID != "registry-test-plugin" || route.Path != "widgets/{name}" {
		t.Fatalf("Match widgets = %q %+v %v", pluginID, route, ok)
	}
	if parameters["name"] != "alpha" {
		t.Fatalf("parameters = %v", parameters)
	}

	pluginID, route, _, ok = Match([]string{"plugins", "registry-test-plugin"})
	if !ok || pluginID != "registry-test-plugin" || route.Path != "" {
		t.Fatalf("Match plugin root = %q %+v %v", pluginID, route, ok)
	}

	if _, _, _, ok = Match([]string{"plugins", "registry-test-plugin", "missing"}); ok {
		t.Fatal("Match returned a route for an unknown path")
	}
	if _, _, _, ok = Match([]string{"registry-test-plugin", "widgets", "alpha"}); ok {
		t.Fatal("Match accepted a plugin path outside /api/v1/plugins")
	}
}

func TestRegisterRejectsInvalidPlugins(t *testing.T) {
	assertPanics(t, "empty id", func() {
		Register("", Route{Path: "x", Methods: []string{http.MethodGet}, Handle: func(Context) {}})
	})
	registerAPIForTest(t, "repositories", Route{Path: "x", Methods: []string{http.MethodGet}, Handle: func(Context) {}})
	registerAPIForTest(t, "gc", Route{Path: "run", Methods: []string{http.MethodPost}, Handle: func(Context) {}})
	assertPanics(t, "no routes", func() { Register("registry-test-empty") })
	assertPanics(t, "nil handle", func() {
		Register("registry-test-nil", Route{Path: "x", Methods: []string{http.MethodGet}})
	})
	registerAPIForTest(t, "registry-test-dup", Route{
		Path:    "x",
		Methods: []string{http.MethodGet},
		Handle:  func(Context) {},
	})
	assertPanics(t, "duplicate plugin", func() {
		Register("registry-test-dup", Route{
			Path:       "y",
			Methods:    []string{http.MethodGet},
			Handle:     func(Context) {},
			Operations: map[string]Operation{http.MethodGet: testOperation()},
		})
	})
}

func TestStaticSegmentsWinOverParameters(t *testing.T) {
	handle := func(Context) {}
	registerAPIForTest(t, "registry-test-precedence", Route{
		Path:    "widgets/{name}",
		Methods: []string{http.MethodGet},
		Handle:  handle,
	}, Route{
		Path:    "widgets/all",
		Methods: []string{http.MethodGet},
		Handle:  handle,
	}, Route{
		Path:    "{kind}/all/items",
		Methods: []string{http.MethodGet},
		Handle:  handle,
	}, Route{
		Path:    "widgets/{name}/items",
		Methods: []string{http.MethodGet},
		Handle:  handle,
	})

	_, route, parameters, ok := Match([]string{"plugins", "registry-test-precedence", "widgets", "all"})
	if !ok || route.Path != "widgets/all" || len(parameters) != 0 {
		t.Fatalf("Match widgets/all = %+v %v %v", route, parameters, ok)
	}
	_, route, parameters, ok = Match([]string{"plugins", "registry-test-precedence", "widgets", "alpha"})
	if !ok || route.Path != "widgets/{name}" || parameters["name"] != "alpha" {
		t.Fatalf("Match widgets/alpha = %+v %v %v", route, parameters, ok)
	}
	_, route, _, ok = Match([]string{"plugins", "registry-test-precedence", "widgets", "all", "items"})
	if !ok || route.Path != "widgets/{name}/items" {
		t.Fatalf("Match widgets/all/items = %+v %v", route, ok)
	}
}

func TestRegisterRejectsParameterOnlyDuplicates(t *testing.T) {
	handle := func(Context) {}
	assertPanics(t, "same shape", func() {
		Register("registry-test-shape", Route{
			Path:       "widgets/{a}",
			Methods:    []string{http.MethodGet},
			Handle:     handle,
			Operations: map[string]Operation{http.MethodGet: testOperation()},
		}, Route{
			Path:       "widgets/{b}",
			Methods:    []string{http.MethodGet},
			Handle:     handle,
			Operations: map[string]Operation{http.MethodGet: testOperation()},
		})
	})
	if Registered("registry-test-shape") {
		t.Fatal("rejected registration was recorded")
	}
}

func TestRoutesAndOperationDocumentation(t *testing.T) {
	handle := func(Context) {}
	registerAPIForTest(t, "registry-test-docs", Route{
		Path:    "widgets/{name}",
		Methods: []string{http.MethodGet, http.MethodDelete},
		Handle:  handle,
		Operations: map[string]Operation{
			http.MethodGet:    {"summary": "Read a widget", "responses": testOperation()["responses"]},
			http.MethodDelete: testOperation(),
		},
	}, Route{
		Path:    "widgets",
		Methods: []string{http.MethodGet},
		Handle:  handle,
	})

	routes := Routes("registry-test-docs")
	if len(routes) != 2 || routes[0].Path != "widgets" || routes[1].Path != "widgets/{name}" {
		t.Fatalf("Routes() = %+v", routes)
	}
	if routes[1].Operations[http.MethodGet]["summary"] != "Read a widget" {
		t.Fatalf("documented operation lost: %+v", routes[1].Operations)
	}
	if Routes("registry-test-unknown") != nil {
		t.Fatal("Routes() for an unknown plugin is not nil")
	}

	assertPanics(t, "operation for unserved method", func() {
		Register("registry-test-docs-bad", Route{
			Path:    "x",
			Methods: []string{http.MethodGet},
			Handle:  handle,
			Operations: map[string]Operation{
				http.MethodGet:  testOperation(),
				http.MethodPost: {"summary": "not served"},
			},
		})
	})
}

func TestRegisterRequiresExplicitSuccessResponses(t *testing.T) {
	for _, route := range []Route{
		{Path: "x", Methods: []string{http.MethodGet}, Handle: func(Context) {}},
		{Path: "x", Methods: []string{http.MethodGet}, Handle: func(Context) {}, Operations: map[string]Operation{http.MethodGet: {"summary": "No response"}}},
		{Path: "x", Methods: []string{http.MethodGet}, Handle: func(Context) {}, Operations: map[string]Operation{http.MethodGet: {"responses": map[string]any{"200": map[string]any{"description": "JSON", "content": map[string]any{"application/json": map[string]any{}}}}}}},
		{Path: "x", Methods: []string{http.MethodPost}, Handle: func(Context) {}, Operations: map[string]Operation{http.MethodPost: {"responses": testOperation()["responses"], "requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{}}}}}},
	} {
		assertPanics(t, "missing success response", func() { Register("registry-test-incomplete", route) })
	}
}

func TestRegisterRejectsReferencedSchemas(t *testing.T) {
	for _, schema := range []map[string]any{
		{"$ref": "#/components/schemas/Missing"},
		{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Missing"}},
	} {
		assertPanics(t, "referenced schema", func() {
			Register("registry-test-schema-ref", Route{
				Path:    "widgets",
				Methods: []string{http.MethodGet},
				Handle:  func(Context) {},
				Operations: map[string]Operation{http.MethodGet: {
					"responses": map[string]any{"200": map[string]any{
						"description": "Widgets",
						"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
					}},
				}},
			})
		})
		if Registered("registry-test-schema-ref") {
			t.Fatal("rejected schema reference was registered")
		}
	}
}

func TestRegisterRejectsNoncanonicalPaths(t *testing.T) {
	for _, path := range []string{
		"/", "/widgets", "widgets/", "widgets//item",
		".", "..", "widgets/./item", "widgets/../item",
		"widgets/%2F", "widgets/%zz", "widgets/%",
		"widgets?query", "widgets#fragment", "widgets\\item", "widgets item",
		"widgets/{}", "widgets/{Bad}", "widgets/{0name}",
		"widgets/{name", "widgets/name}", "widgets/{name}extra",
		"widgets/{name}/{name}",
	} {
		t.Run(path, func(t *testing.T) {
			assertPanics(t, path, func() {
				Register("registry-test-bad-path", Route{
					Path: path, Methods: []string{http.MethodGet}, Handle: func(Context) {},
					Operations: map[string]Operation{http.MethodGet: testOperation()},
				})
			})
			if Registered("registry-test-bad-path") {
				t.Fatal("rejected path was registered")
			}
		})
	}
}

func TestRegisterAcceptsCanonicalPathsAndRoot(t *testing.T) {
	registerAPIForTest(t, "registry-test-canonical", Route{
		Path: "", Methods: []string{http.MethodGet}, Handle: func(Context) {},
	}, Route{
		Path: "v1.0/~items/{item-name}", Methods: []string{http.MethodGet}, Handle: func(Context) {},
	})
	if _, route, _, ok := Match([]string{"plugins", "registry-test-canonical"}); !ok || route.Path != "" {
		t.Fatalf("plugin root match = %+v, %v", route, ok)
	}
	if _, _, _, ok := Match([]string{"plugins", "registry-test-canonical", ""}); ok {
		t.Fatal("root route matched an extra empty segment")
	}
	if _, route, params, ok := Match([]string{"plugins", "registry-test-canonical", "v1.0", "~items", "alpha"}); !ok || route.Path != "v1.0/~items/{item-name}" || params["item-name"] != "alpha" {
		t.Fatalf("canonical route match = %+v, %v, %v", route, params, ok)
	}
	if _, _, _, ok := Match([]string{"plugins", "registry-test-canonical", "v1.0", "~items", ""}); ok {
		t.Fatal("parameter route matched an empty value")
	}
}

func TestRegisterRejectsInvalidAndDuplicateMethods(t *testing.T) {
	for _, method := range []string{"", "get", "Get", "CUSTOM", "CONNECT", "GET ", " GET"} {
		t.Run(method, func(t *testing.T) {
			assertPanics(t, method, func() {
				Register("registry-test-bad-method", Route{
					Path: "x", Methods: []string{method}, Handle: func(Context) {},
					Operations: map[string]Operation{method: testOperation()},
				})
			})
		})
	}
	assertPanics(t, "duplicate method", func() {
		Register("registry-test-duplicate-method", Route{
			Path: "x", Methods: []string{http.MethodGet, http.MethodGet}, Handle: func(Context) {},
			Operations: map[string]Operation{http.MethodGet: testOperation()},
		})
	})
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace,
	} {
		if !validMethod(method) {
			t.Errorf("canonical method %q rejected", method)
		}
	}
}

func TestRoutesAndMatchCannotMutateRegistry(t *testing.T) {
	const pluginID = "registry-test-isolation"
	methods := []string{http.MethodGet, http.MethodPost}
	operation := Operation{
		"responses": map[string]any{"200": map[string]any{
			"description": "Original",
			"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{"value": map[string]any{
						"type": "string", "enum": []any{"original"},
					}},
				},
			}},
		}},
	}
	registerAPIForTest(t, pluginID, Route{
		Path: "status", Methods: methods, Handle: func(Context) {},
		Operations: map[string]Operation{http.MethodGet: operation, http.MethodPost: testOperation()},
	})
	methods[0] = http.MethodDelete
	operation["responses"].(map[string]any)["200"].(map[string]any)["description"] = "Input changed"

	for _, route := range []Route{Routes(pluginID)[0], matchedRoute(t, pluginID)} {
		route.Methods[0] = http.MethodDelete
		route.Operations[http.MethodGet]["responses"].(map[string]any)["200"].(map[string]any)["description"] = "Output changed"
		property := route.Operations[http.MethodGet]["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["value"].(map[string]any)
		property["enum"].([]any)[0] = "changed"
		delete(route.Operations, http.MethodPost)
	}
	for _, route := range []Route{Routes(pluginID)[0], matchedRoute(t, pluginID)} {
		if !slices.Equal(route.Methods, []string{http.MethodGet, http.MethodPost}) || !route.Allows(http.MethodGet) || route.Allows(http.MethodDelete) {
			t.Fatalf("registered methods changed: %v", route.Methods)
		}
		if route.Operations[http.MethodPost] == nil {
			t.Fatal("registered POST operation was deleted")
		}
		response := route.Operations[http.MethodGet]["responses"].(map[string]any)["200"].(map[string]any)
		if response["description"] != "Original" {
			t.Fatalf("registered response changed: %v", response)
		}
		property := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["value"].(map[string]any)
		if property["enum"].([]any)[0] != "original" {
			t.Fatalf("registered schema changed: %v", property)
		}
	}
}

func TestConcurrentRouteReadsRemainIndependent(t *testing.T) {
	const pluginID = "registry-test-concurrent"
	registerAPIForTest(t, pluginID, Route{
		Path: "status", Methods: []string{http.MethodGet}, Handle: func(Context) {},
	})
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				route := Routes(pluginID)[0]
				_, matched, _, found := Match([]string{"plugins", pluginID, "status"})
				if !found {
					t.Error("concurrent match did not find registered route")
					return
				}
				if !route.Allows(http.MethodGet) || !matched.Allows(http.MethodGet) {
					t.Error("concurrent read observed mutated methods")
					return
				}
				route.Methods[0] = http.MethodDelete
				matched.Methods[0] = http.MethodDelete
				route.Operations[http.MethodGet]["summary"] = "changed"
				matched.Operations[http.MethodGet]["summary"] = "changed"
			}
		}()
	}
	workers.Wait()
	if route := matchedRoute(t, pluginID); !route.Allows(http.MethodGet) || route.Operations[http.MethodGet]["summary"] != nil {
		t.Fatalf("registered route changed after concurrent reads: %+v", route)
	}
}

func matchedRoute(t *testing.T, pluginID string) Route {
	t.Helper()
	_, route, _, ok := Match([]string{"plugins", pluginID, "status"})
	if !ok {
		t.Fatal("registered status route did not match")
	}
	return route
}

func assertPanics(t *testing.T, name string, callback func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: Register did not panic", name)
		}
	}()
	callback()
}
