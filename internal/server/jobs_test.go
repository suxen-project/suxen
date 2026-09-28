package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/httpx"
	spiapi "github.com/suxen-project/suxen/spi/api"
)

func TestScheduledJobsAreRunnableAndReserved(t *testing.T) {
	for _, name := range []string{"gc", "cleanup"} {
		job, found := scheduledJobByName(name)
		if !found || job.name != name {
			t.Fatalf("scheduledJobByName(%q) = %+v, %v", name, job, found)
		}
		if job.run == nil || job.interval == nil {
			t.Fatalf("job %q is missing run or interval", name)
		}
		if job.leaseName == "" || job.metricRole == "" {
			t.Fatalf("job %q is missing lease name or metric role", name)
		}
	}
	seen := make(map[string]struct{}, len(scheduledJobs))
	for _, job := range scheduledJobs {
		for _, key := range []string{job.name, "lease:" + job.leaseName, "role:" + job.metricRole} {
			if _, dup := seen[key]; dup {
				t.Fatalf("scheduled job identifier %q is used twice", key)
			}
			seen[key] = struct{}{}
		}
	}
}

func TestPluginAPIRoutesAreNamespaced(t *testing.T) {
	t.Parallel()
	spiapi.Register("apitest", spiapi.Route{
		Path:    "ping",
		Methods: []string{http.MethodGet},
		Handle: func(ctx spiapi.Context) {
			ctx.WriteJSON(http.StatusOK, map[string]string{
				"plugin": ctx.PluginID(),
			})
		},
		Operations: map[string]spiapi.Operation{http.MethodGet: testPluginOperation()},
	}, spiapi.Route{
		Path:    "echo",
		Methods: []string{http.MethodPost},
		Handle: func(ctx spiapi.Context) {
			var payload map[string]string
			if !ctx.DecodeJSON(&payload) {
				return
			}
			ctx.WriteJSON(http.StatusOK, payload)
		},
		Operations: map[string]spiapi.Operation{http.MethodPost: testPluginOperation()},
	})
	t.Cleanup(func() {
		spiapi.Unregister("apitest")
	})

	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/plugins/apitest/ping", nil, true)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["plugin"] != "apitest" {
		t.Fatalf("plugin response = %v", body)
	}

	echoed := fixture.request(t, http.MethodPost, "/api/v1/plugins/apitest/echo", []byte(`{"hello":"world"}`), true)
	assertStatus(t, echoed, http.StatusOK)
	var echo map[string]string
	if err := json.NewDecoder(echoed.Body).Decode(&echo); err != nil {
		t.Fatal(err)
	}
	echoed.Body.Close()
	if echo["hello"] != "world" {
		t.Fatalf("echo response = %v", echo)
	}

	invalid := fixture.request(t, http.MethodPost, "/api/v1/plugins/apitest/echo", []byte(`{"hello":`), true)
	assertStatus(t, invalid, http.StatusBadRequest)
	invalid.Body.Close()

	oversized := fixture.request(t, http.MethodPost, "/api/v1/plugins/apitest/echo", []byte(`{"pad":"`+strings.Repeat("x", 1<<20)+`"}`), true)
	assertStatus(t, oversized, http.StatusBadRequest)
	oversized.Body.Close()

	missing := fixture.request(t, http.MethodGet, "/api/v1/plugins/apitest/missing", nil, true)
	assertStatus(t, missing, http.StatusNotFound)
	missing.Body.Close()

	unauth := fixture.request(t, http.MethodGet, "/api/v1/plugins/apitest/ping", nil, false)
	assertStatus(t, unauth, http.StatusUnauthorized)
	unauth.Body.Close()
}

func TestPluginAPIPrivilegeMatchesPluginID(t *testing.T) {
	spiapi.Register("privtest", spiapi.Route{
		Path:    "ping",
		Methods: []string{http.MethodGet, http.MethodPost},
		Handle: func(ctx spiapi.Context) {
			ctx.WriteJSON(http.StatusOK, map[string]string{"ok": "true"})
		},
		Operations: map[string]spiapi.Operation{
			http.MethodGet:  testPluginOperation(),
			http.MethodPost: testPluginOperation(),
		},
	})
	t.Cleanup(func() {
		spiapi.Unregister("privtest")
	})

	if got := controlPlanePrivilegeRequirement(httpx.SplitPath("plugins/privtest/ping"), http.MethodGet); got != "admin:plugin-privtest:read" {
		t.Fatalf("GET privilege = %q", got)
	}
	if got := controlPlanePrivilegeRequirement(httpx.SplitPath("plugins/privtest/ping"), http.MethodPost); got != "admin:plugin-privtest:write" {
		t.Fatalf("POST privilege = %q", got)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/privileges", nil)
	(&Server{}).handlePrivileges(recorder, request)
	var catalog struct {
		AdminResources []string `json:"adminResources"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(catalog.AdminResources, "plugin-privtest") {
		t.Fatalf("admin resources = %v, want privtest", catalog.AdminResources)
	}
}

func TestPluginPrivilegeDoesNotShareCoreResourceGrant(t *testing.T) {
	spiapi.Register("users", spiapi.Route{
		Path:       "ping",
		Methods:    []string{http.MethodGet},
		Handle:     func(spiapi.Context) {},
		Operations: map[string]spiapi.Operation{http.MethodGet: testPluginOperation()},
	})
	t.Cleanup(func() { spiapi.Unregister("users") })
	plugin := controlPlanePrivilegeRequirement(httpx.SplitPath("plugins/users/ping"), http.MethodGet)
	core := controlPlanePrivilegeRequirement(httpx.SplitPath("users"), http.MethodGet)
	if plugin != "admin:plugin-users:read" || core != "admin:users:read" {
		t.Fatalf("plugin privilege = %q, core privilege = %q", plugin, core)
	}
}

func TestCoreControlPlaneRoutesStayOutsidePluginNamespace(t *testing.T) {
	for _, route := range controlPlaneRoutes {
		parts := httpx.SplitPath(route.path)
		if len(parts) < 2 || parts[0] != "api" || parts[1] != "v1" {
			t.Fatalf("control-plane route %q is not under /api/v1", route.path)
		}
		if len(parts) == 2 {
			continue
		}
		first := parts[2]
		if first[0] == '{' {
			continue
		}
		if first == "plugins" {
			t.Errorf("core route %s occupies the plugin namespace", route.path)
		}
	}
}

func testPluginOperation() spiapi.Operation {
	return spiapi.Operation{"responses": map[string]any{"204": map[string]any{"description": "Complete"}}}
}
