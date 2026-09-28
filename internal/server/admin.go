package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/suxen-project/suxen/internal/httpx"
	spiapi "github.com/suxen-project/suxen/spi/api"
)

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	adminPath := strings.TrimPrefix(r.URL.Path, "/api/v1")
	parts := httpx.SplitPath(adminPath)
	if len(parts) > 1 && parts[0] == "repositories" {
		httpx.SetRequestLogRepository(w, parts[1])
	}
	requiredPrivilege := controlPlanePrivilegeRequirement(parts, r.Method)
	if requiredPrivilege != publicPrivilegeRequirement &&
		requiredPrivilege != visibleRepositoryPrivilegeRequirement &&
		!s.identity.RequireSessionPrivilege(w, r, requiredPrivilege) {
		return
	}

	route, parameters, found := matchControlPlaneRoute(parts)
	if !found {
		pluginID, pluginRoute, pluginParameters, pluginFound := spiapi.Match(parts)
		if pluginFound {
			if !pluginRoute.Allows(r.Method) {
				httpx.MethodNotAllowed(w, pluginRoute.Methods...)
				return
			}
			pluginRoute.Handle(pluginAPIContext{
				pluginID:   pluginID,
				parameters: pluginParameters,
				writer:     w,
				request:    r,
			})
			return
		}
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "admin route not found")
		return
	}
	if !route.allows(r.Method) {
		httpx.MethodNotAllowed(w, route.methods...)
		return
	}
	route.handle(s, w, r, parameters)
}

const (
	publicPrivilegeRequirement            = "public"
	visibleRepositoryPrivilegeRequirement = "repository:<visible>:read"
)

// controlPlanePrivilegeRequirement returns the stable privilege expression for
// a control-plane operation. The visible-repository expression means that the
// handler filters results using each repository's read privilege.
func controlPlanePrivilegeRequirement(parts []string, method string) string {
	if len(parts) == 0 {
		return publicPrivilegeRequirement
	}
	if len(parts) >= 2 && parts[0] == "plugins" {
		return pluginPrivilege(parts[1], method)
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "whoami":
			return publicPrivilegeRequirement
		case "browse", "search":
			return visibleRepositoryPrivilegeRequirement
		case "repositories":
			if method == http.MethodGet || method == http.MethodHead {
				return visibleRepositoryPrivilegeRequirement
			}
		}
	}
	if len(parts) == 2 &&
		parts[0] == "repositories" &&
		parts[1] != "" &&
		(method == http.MethodGet || method == http.MethodHead) {
		return fmt.Sprintf("repository:%s:read", parts[1])
	}
	if len(parts) == 3 &&
		parts[0] == "repositories" &&
		parts[2] == "browse" {
		return fmt.Sprintf("repository:%s:read", parts[1])
	}
	if action, ok := repositorySubresourceAction(parts, method); ok {
		return fmt.Sprintf("repository:%s:%s", parts[1], action)
	}
	// Per-store garbage collection deletes stored data, so it is gated by the same
	// admin:gc:run privilege as the global collector rather than blob-store writes.
	if len(parts) == 3 && parts[0] == "blob-stores" && parts[2] == "gc" {
		return "admin:gc:run"
	}
	return adminPrivilege(parts, method)
}

// repositorySubresourceAction maps repository-owned control-plane resources to
// the repository privilege action that protects them. Repository collection GET
// and item GET use repository read (see controlPlanePrivilegeRequirement).
// Mutations of the repository resource itself remain global administration.
func repositorySubresourceAction(parts []string, method string) (string, bool) {
	if len(parts) < 3 || parts[0] != "repositories" || parts[1] == "" {
		return "", false
	}

	switch {
	case len(parts) == 3 && parts[2] == "assets" && method == http.MethodGet:
		return "read", true
	case len(parts) == 3 && parts[2] == "components" && method == http.MethodGet:
		return "read", true
	case len(parts) == 5 && parts[2] == "assets" &&
		(parts[4] == "manifest" || parts[4] == "download") && method == http.MethodGet:
		return "read", true
	case len(parts) == 4 && parts[2] == "assets":
		if method == http.MethodDelete {
			return "delete", true
		}
		if method == http.MethodGet {
			return "read", true
		}
	case len(parts) == 6 && parts[2] == "assets" && parts[4] == "attributes":
		if method == http.MethodGet {
			return "read", true
		}
		if method == http.MethodPut || method == http.MethodDelete {
			return "annotate", true
		}
	case len(parts) == 5 && parts[2] == "assets" && parts[4] == "verification":
		if method == http.MethodPost {
			return "annotate", true
		}
	case len(parts) == 3 && isRepositoryPolicyResource(parts[2]):
		if method == http.MethodGet {
			return "read", true
		}
		if method == http.MethodPut || method == http.MethodDelete {
			return "manage", true
		}
	case len(parts) == 3 && parts[2] == "cleanup" && method == http.MethodPost:
		return "delete", true
	}

	return "", false
}

func isRepositoryPolicyResource(resource string) bool {
	return resource == "classification" ||
		resource == "download-gate" ||
		resource == "trust-policy"
}

var adminPrivilegeResourceByPath = map[string]string{
	"provision":               "provision",
	"blob-stores":             "blob-stores",
	"download-gate-defaults":  "download-gate-defaults",
	"trust-policy-defaults":   "trust-policy-defaults",
	"classification-defaults": "classification-defaults",
	"repositories":            "repositories",
	"stats":                   "stats",
	"usage":                   "stats",
	"users":                   "users",
	"roles":                   "roles",
	"privileges":              "privileges",
	"oidc-providers":          "oidc-providers",
	"cleanup-policies":        "cleanup-policies",
	"webhooks":                "webhooks",
	"tasks":                   "tasks",
	"gc":                      "gc",
	"verify":                  "verify",
}

// adminPrivilege uses a stable semantic vocabulary instead of deriving privilege
// resource names from public URL spelling.
func adminPrivilege(parts []string, method string) string {
	resource := "access"
	if len(parts) > 0 {
		if mapped, ok := adminPrivilegeResourceByPath[parts[0]]; ok {
			resource = mapped
		}
	}
	action := "write"
	if method == http.MethodGet || method == http.MethodHead {
		action = "read"
	}
	if (resource == "gc" || resource == "verify") &&
		method != http.MethodGet && method != http.MethodHead {
		action = "run"
	}
	return fmt.Sprintf("admin:%s:%s", resource, action)
}

func pluginPrivilege(pluginID, method string) string {
	action := "write"
	if method == http.MethodGet || method == http.MethodHead {
		action = "read"
	}
	return fmt.Sprintf("admin:plugin-%s:%s", pluginID, action)
}

func (s *Server) handlePrivileges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	adminResources := []string{
		"provision",
		"blob-stores",
		"download-gate-defaults",
		"trust-policy-defaults",
		"classification-defaults",
		"repositories",
		"roles",
		"users",
		"privileges",
		"oidc-providers",
		"cleanup-policies",
		"webhooks",
		"stats",
		"tasks",
		"gc",
		"verify",
	}
	for _, pluginID := range spiapi.Names() {
		adminResources = append(adminResources, "plugin-"+pluginID)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"syntax":            "repository:<name|*>:<read|write|delete|annotate|manage> or admin:<resource|*>:<action|*>",
		"repositoryActions": []string{"read", "write", "delete", "annotate", "manage"},
		"adminResources":    adminResources,
	})
}
