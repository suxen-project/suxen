package server

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/contract"
	"github.com/suxen-project/suxen/internal/httpx"
	spiapi "github.com/suxen-project/suxen/spi/api"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

//go:embed openapi.json
var openAPITemplate []byte

type apiDiscoveryResponse struct {
	Versions       []apiVersionDiscovery     `json:"versions"`
	Contract       []contract.SurfaceVersion `json:"contract"`
	Mounts         apiMountDiscovery         `json:"mounts"`
	Formats        []string                  `json:"formats"`
	Plugins        []string                  `json:"plugins"`
	AttributePaths []string                  `json:"attributePaths"`
}

type apiVersionDiscovery struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	OpenAPI string `json:"openapi"`
	Version string `json:"version"`
}

type apiMountDiscovery struct {
	DefaultOCI   string `json:"defaultOCI"`
	Repositories string `json:"repositories"`
	PluginRoutes string `json:"pluginRoutes"`
}

func (s *Server) handleAPIDiscovery(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, apiDiscoveryResponse{
		Versions: []apiVersionDiscovery{{
			Name:    "v1",
			Path:    "/api/v1",
			OpenAPI: "/api/openapi.json",
			Version: contract.HTTPAPIVersion(),
		}},
		Contract: contract.Matrix(),
		Mounts: apiMountDiscovery{
			DefaultOCI:   "/v2/",
			Repositories: "/repository/{name}/",
			PluginRoutes: "/api/v1/plugins/{pluginID}/",
		},
		Formats:        spiformat.Names(),
		Plugins:        spiapi.Names(),
		AttributePaths: assetattrs.KnownAttributePaths(),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !httpx.AllowReadOnlyMethod(w, r) {
		return
	}
	if !s.identity.RequirePrivilege(w, r, "admin:stats:read") {
		return
	}
	s.metrics.handler.ServeHTTP(w, r)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if !httpx.AllowReadOnlyMethod(w, r) {
		return
	}

	var document map[string]any
	if err := json.Unmarshal(openAPITemplate, &document); err != nil {
		httpx.WriteProblem(
			w,
			http.StatusInternalServerError,
			"openapi_unavailable",
			"embedded OpenAPI document is invalid",
		)
		return
	}
	document["info"].(map[string]any)["version"] = contract.HTTPAPIVersion()
	if err := documentPluginRoutes(document); err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"openapi_unavailable",
			"plugin route documentation is invalid",
			err,
		)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, document)
}
