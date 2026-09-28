package server

import (
	"github.com/suxen-project/suxen/internal/httpx"
)

import "net/http"

type controlPlaneHandler func(
	*Server,
	http.ResponseWriter,
	*http.Request,
	map[string]string,
)

// controlPlaneRoute describes one dispatchable control-plane resource. Keeping
// paths and methods together gives contract tests a precise router surface while
// handlers remain responsible for resource-specific behavior.
type controlPlaneRoute struct {
	path    string
	methods []string
	handle  controlPlaneHandler
}

func (route controlPlaneRoute) allows(method string) bool {
	for _, allowed := range route.methods {
		if method == allowed {
			return true
		}
	}
	return false
}

var controlPlaneRoutes = []controlPlaneRoute{
	{
		path:    "/api/v1",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleAPIDiscovery),
	},
	{
		path:    "/api/v1/provision",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleProvision),
	},
	{
		path:    "/api/v1/blob-stores",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleBlobStoreCollection),
	},
	{
		path:    "/api/v1/blob-stores/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleBlobStoreItem, "name"),
	},
	{
		path:    "/api/v1/blob-stores/{name}/drain",
		methods: []string{http.MethodPost, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleBlobStoreDrain, "name"),
	},
	{
		path:    "/api/v1/blob-stores/{name}/usage",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleBlobStoreUsage, "name"),
	},
	{
		path:    "/api/v1/blob-stores/{name}/gc",
		methods: []string{http.MethodPost},
		handle:  routeWithParameter((*Server).handleBlobStoreGC, "name"),
	},
	{
		path:    "/api/v1/download-gate-defaults",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithoutParameters((*Server).handleDownloadGateDefaults),
	},
	{
		path:    "/api/v1/trust-policy-defaults",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithoutParameters((*Server).handleTrustPolicyDefaults),
	},
	{
		path:    "/api/v1/classification-defaults",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithoutParameters((*Server).handleClassificationDefaults),
	},
	{
		path:    "/api/v1/whoami",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleWhoAmI),
	},
	{
		path:    "/api/v1/browse",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleBrowseRepositories),
	},
	{
		path:    "/api/v1/search",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleSearch),
	},
	{
		path:    "/api/v1/repositories",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleRepositoryCollection),
	},
	{
		path:    "/api/v1/repositories/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleRepositoryItem, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/assets",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleAssets, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/browse",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleRepositoryBrowse, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/components",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleRepositoryComponents, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/assets/{id}",
		methods: []string{http.MethodGet, http.MethodDelete},
		handle: routeWithParameters(
			(*Server).handleAssetItem,
			"name",
			"id",
		),
	},
	{
		path:    "/api/v1/repositories/{name}/assets/{id}/manifest",
		methods: []string{http.MethodGet},
		handle: routeWithParameters(
			(*Server).handleAssetManifest,
			"name",
			"id",
		),
	},
	{
		path:    "/api/v1/repositories/{name}/assets/{id}/download",
		methods: []string{http.MethodGet},
		handle: routeWithParameters(
			(*Server).handleAssetDownload,
			"name",
			"id",
		),
	},
	{
		path:    "/api/v1/repositories/{name}/classification",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleClassification, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/download-gate",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleDownloadGate, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/trust-policy",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleTrustPolicy, "name"),
	},
	{
		path:    "/api/v1/repositories/{name}/cleanup",
		methods: []string{http.MethodPost},
		handle:  routeWithParameter((*Server).handleRepositoryCleanup, "name"),
	},
	{
		path: "/api/v1/repositories/{name}/assets/{id}/attributes/{namespace}",
		methods: []string{
			http.MethodGet,
			http.MethodPut,
			http.MethodDelete,
		},
		handle: routeWithThreeParameters(
			(*Server).handleAssetAttributes,
			"name",
			"id",
			"namespace",
		),
	},
	{
		path:    "/api/v1/repositories/{name}/assets/{id}/verification",
		methods: []string{http.MethodPost},
		handle: routeWithParameters(
			(*Server).handleAssetVerification,
			"name",
			"id",
		),
	},
	{
		path:    "/api/v1/stats",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleStats),
	},
	{
		path:    "/api/v1/usage",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleStorageUsage),
	},
	{
		path:    "/api/v1/users",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleUsers),
	},
	{
		path:    "/api/v1/users/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleUserItem, "name"),
	},
	{
		path:    "/api/v1/users/{name}/tokens",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithParameter((*Server).handleUserTokens, "name"),
	},
	{
		path:    "/api/v1/users/{name}/tokens/{id}",
		methods: []string{http.MethodDelete},
		handle: routeWithParameters(
			(*Server).handleUserTokenItem,
			"name",
			"id",
		),
	},
	{
		path:    "/api/v1/users/{name}/roles",
		methods: []string{http.MethodGet, http.MethodPut},
		handle:  routeWithParameter((*Server).handleUserRoles, "name"),
	},
	{
		path:    "/api/v1/roles",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleRoleCollection),
	},
	{
		path:    "/api/v1/roles/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleRoleItem, "name"),
	},
	{
		path:    "/api/v1/privileges",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handlePrivileges),
	},
	{
		path:    "/api/v1/oidc-providers",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleOIDCProviderCollection),
	},
	{
		path:    "/api/v1/oidc-providers/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleOIDCProviderItem, "name"),
	},
	{
		path:    "/api/v1/cleanup-policies",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleCleanupPolicyCollection),
	},
	{
		path:    "/api/v1/cleanup-policies/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleCleanupPolicyItem, "name"),
	},
	{
		path:    "/api/v1/cleanup-policies/{name}/run",
		methods: []string{http.MethodPost},
		handle:  routeWithParameter((*Server).handleCleanupPolicyRun, "name"),
	},
	{
		path:    "/api/v1/webhooks",
		methods: []string{http.MethodGet, http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleWebhookCollection),
	},
	{
		path:    "/api/v1/webhooks/{name}",
		methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete},
		handle:  routeWithParameter((*Server).handleWebhookItem, "name"),
	},
	{
		path:    "/api/v1/webhooks/{name}/deliveries",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleWebhookDeliveries, "name"),
	},
	{
		path:    "/api/v1/gc",
		methods: []string{http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleGarbageCollection),
	},
	{
		path:    "/api/v1/verify",
		methods: []string{http.MethodPost},
		handle:  routeWithoutParameters((*Server).handleBlobStoreVerify),
	},
	{
		path:    "/api/v1/tasks/leader",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleSchedulerLeader),
	},
	{
		path:    "/api/v1/tasks",
		methods: []string{http.MethodGet},
		handle:  routeWithoutParameters((*Server).handleTasks),
	},
	{
		path:    "/api/v1/tasks/{id}",
		methods: []string{http.MethodGet},
		handle:  routeWithParameter((*Server).handleTaskItem, "id"),
	},
}

func routeWithoutParameters(
	handler func(*Server, http.ResponseWriter, *http.Request),
) controlPlaneHandler {
	return func(
		server *Server,
		writer http.ResponseWriter,
		request *http.Request,
		_ map[string]string,
	) {
		handler(server, writer, request)
	}
}

func routeWithParameter(
	handler func(*Server, http.ResponseWriter, *http.Request, string),
	parameter string,
) controlPlaneHandler {
	return func(
		server *Server,
		writer http.ResponseWriter,
		request *http.Request,
		parameters map[string]string,
	) {
		handler(server, writer, request, parameters[parameter])
	}
}

func routeWithParameters(
	handler func(*Server, http.ResponseWriter, *http.Request, string, string),
	firstParameter string,
	secondParameter string,
) controlPlaneHandler {
	return func(
		server *Server,
		writer http.ResponseWriter,
		request *http.Request,
		parameters map[string]string,
	) {
		handler(
			server,
			writer,
			request,
			parameters[firstParameter],
			parameters[secondParameter],
		)
	}
}

func routeWithThreeParameters(
	handler func(
		*Server,
		http.ResponseWriter,
		*http.Request,
		string,
		string,
		string,
	),
	firstParameter string,
	secondParameter string,
	thirdParameter string,
) controlPlaneHandler {
	return func(
		server *Server,
		writer http.ResponseWriter,
		request *http.Request,
		parameters map[string]string,
	) {
		handler(
			server,
			writer,
			request,
			parameters[firstParameter],
			parameters[secondParameter],
			parameters[thirdParameter],
		)
	}
}

func matchControlPlaneRoute(parts []string) (controlPlaneRoute, map[string]string, bool) {
	for _, route := range controlPlaneRoutes {
		parameters, matches := matchControlPlanePath(route.path, parts)
		if matches {
			return route, parameters, true
		}
	}
	return controlPlaneRoute{}, nil, false
}

func matchControlPlanePath(pattern string, parts []string) (map[string]string, bool) {
	patternParts := httpx.SplitPath(pattern)
	if len(patternParts) != len(parts)+2 ||
		patternParts[0] != "api" ||
		patternParts[1] != "v1" {
		return nil, false
	}

	parameters := make(map[string]string)
	for index, actual := range parts {
		patternPart := patternParts[index+2]
		if isPathParameter(patternPart) {
			parameters[patternPart[1:len(patternPart)-1]] = actual
			continue
		}
		if patternPart != actual {
			return nil, false
		}
	}
	return parameters, true
}

func isPathParameter(part string) bool {
	return len(part) >= 2 && part[0] == '{' && part[len(part)-1] == '}'
}
