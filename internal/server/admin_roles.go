package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

type roleRequest struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Privileges    []string `json:"privileges"`
	IncludedRoles []string `json:"includedRoles,omitempty"`
}

type roleUpdateRequest struct {
	Description   string   `json:"description,omitempty"`
	Privileges    []string `json:"privileges"`
	IncludedRoles []string `json:"includedRoles,omitempty"`
}

func (request roleUpdateRequest) domainRole(name string) domain.Role {
	return roleRequest{
		Description: request.Description,
		Privileges:  request.Privileges,
	}.domainRole(name)
}

func (request roleRequest) domainRole(name string) domain.Role {
	if name == "" {
		name = request.Name
	}
	return domain.Role{
		Name:        name,
		Description: request.Description,
		Privileges:  request.Privileges,
	}
}

// roleCommands is the role-mutation capability the role handlers consume from
// the control plane: create/update and delete, each folding the ownership record
// into one transaction. It keeps the handlers off the aggregate control plane.
type roleCommands interface {
	SaveRole(context.Context, controlplane.SaveRoleCommand) error
	DeleteRole(context.Context, string, controlplane.Intent) error
}

// roleReads is the role read capability the role handlers need: one role by name
// and the full list.
type roleReads interface {
	Role(context.Context, string) (domain.Role, error)
	Roles(context.Context) ([]domain.Role, error)
}

// roleReads narrows the metadata store to the role read capability. It reads
// s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) roleReads() roleReads {
	return s.metadata
}

func (s *Server) handleRoleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		roles, err := s.roleReads().Roles(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "role")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range roles {
			roles[index].Managed = managedName(managed, roles[index].Name)
		}
		httpx.WriteCollection(w, r, "roles", roles, func(role domain.Role) string {
			return role.Name
		})
	case http.MethodPost:
		var request roleRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if len(request.IncludedRoles) > 0 {
			httpx.WriteResult(w, nil, domain.ErrNestedRolesUnsupported)
			return
		}
		role := request.domainRole("")
		if err := s.roles.SaveRole(r.Context(), controlplane.SaveRoleCommand{
			Role:   role,
			Create: true,
			Intent: controlplane.Imperative(false),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		created, err := s.roleReads().Role(r.Context(), role.Name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleRoleItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		role, err := s.roleReads().Role(r.Context(), name)
		if err == nil {
			role.Managed, err = s.resourceIsManaged(r.Context(), "role", name)
		}
		httpx.WriteResult(w, role, err)
	case http.MethodPut:
		var request roleUpdateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if len(request.IncludedRoles) > 0 {
			httpx.WriteResult(w, nil, domain.ErrNestedRolesUnsupported)
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		role := request.domainRole(name)
		_, err = s.roleReads().Role(r.Context(), name)
		creating := errors.Is(err, domain.ErrNotFound)
		if err != nil && !creating {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.roles.SaveRole(r.Context(), controlplane.SaveRoleCommand{
			Role:   role,
			Create: creating,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.roleReads().Role(r.Context(), name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if creating {
			httpx.WriteCreated(w, r.URL.Path, stored)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, stored)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.roles.DeleteRole(r.Context(), name, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}
