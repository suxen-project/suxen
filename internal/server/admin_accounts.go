package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

type userRequest struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Admin    bool     `json:"admin"`
	Roles    []string `json:"roles,omitempty"`
}

type userUpdateRequest struct {
	Password string    `json:"password,omitempty"`
	Admin    *bool     `json:"admin,omitempty"`
	Roles    *[]string `json:"roles,omitempty"`
}

type userRolesRequest struct {
	Roles *[]string `json:"roles"`
}

type userResponse struct {
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	Managed   bool      `json:"managed"`
	CreatedAt time.Time `json:"createdAt"`
	Roles     []string  `json:"roles"`
}

// accountCommands is the account-mutation capability the user handlers consume
// from the control plane: create/update with roles, roles-only replacement, and
// delete, each folding the ownership record into one transaction. It keeps the
// handlers off the aggregate control-plane service.
type accountCommands interface {
	SaveUser(context.Context, controlplane.SaveUserCommand) error
	SetUserRoles(context.Context, string, []string, controlplane.Intent) error
	DeleteUser(context.Context, string, controlplane.Intent) error
}

// accountStore is the account-store surface the server accesses directly: read a
// user by name, list users, read a user's role names, and the first-run
// bootstrap-admin creation. Ordinary user mutations go through accountCommands.
type accountStore interface {
	User(context.Context, string) (domain.User, error)
	Users(context.Context) ([]domain.User, error)
	UserRoles(context.Context, string) ([]string, error)
	CreateBootstrapAdmin(context.Context, string, string, string) error
}

// accountStore narrows the metadata store to the account-store surface. It reads
// s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) accountStore() accountStore {
	return s.metadata
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.accountStore().Users(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "user")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range users {
			users[index].Managed = managedName(managed, users[index].Username)
		}
		httpx.WriteCollection(w, r, "users", users, func(user domain.User) string {
			return user.Username
		})
	case http.MethodPost:
		var request userRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if err := s.validateRoles(r, request.Roles); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		err := s.accounts.SaveUser(r.Context(), controlplane.SaveUserCommand{
			Username: request.Username,
			Password: request.Password,
			Admin:    request.Admin,
			Roles:    request.Roles,
			Create:   true,
			Intent:   controlplane.Imperative(false),
		})
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		created, err := s.userWithRoles(r, request.Username)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, collectionItemLocation(r, request.Username), created)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleUserItem(
	w http.ResponseWriter,
	r *http.Request,
	username string,
) {
	switch r.Method {
	case http.MethodGet:
		s.writeUserWithRoles(w, r, username)
	case http.MethodPut:
		var request userUpdateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		_, err = s.accountStore().User(r.Context(), username)
		creating := errors.Is(err, domain.ErrNotFound)
		if err != nil && !creating {
			httpx.WriteResult(w, nil, err)
			return
		}
		admin := false
		if request.Admin != nil {
			admin = *request.Admin
		}
		if request.Roles != nil {
			if err := s.validateRoles(r, *request.Roles); err != nil {
				httpx.WriteResult(w, nil, err)
				return
			}
		}
		if creating && request.Password == "" {
			httpx.WriteProblem(w, http.StatusBadRequest, "password_required", "password is required when creating a user")
			return
		}
		// nil Roles on update keeps existing assignments; create starts empty.
		var roles []string
		if request.Roles != nil {
			roles = *request.Roles
			if roles == nil {
				roles = []string{}
			}
		} else if creating {
			roles = []string{}
		}
		if err := s.accounts.SaveUser(r.Context(), controlplane.SaveUserCommand{
			Username:      username,
			Password:      request.Password,
			Admin:         admin,
			PreserveAdmin: !creating && request.Admin == nil,
			Roles:         roles,
			Create:        creating,
			Intent:        controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.userWithRoles(r, username)
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
		if err := s.accounts.DeleteUser(r.Context(), username, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) writeUserWithRoles(
	w http.ResponseWriter,
	r *http.Request,
	username string,
) {
	user, err := s.userWithRoles(r, username)
	httpx.WriteResult(w, user, err)
}

func (s *Server) userWithRoles(
	r *http.Request,
	username string,
) (userResponse, error) {
	user, err := s.accountStore().User(r.Context(), username)
	if err != nil {
		return userResponse{}, err
	}
	roles, err := s.accountStore().UserRoles(r.Context(), username)
	if err != nil {
		return userResponse{}, err
	}
	if roles == nil {
		roles = []string{}
	}
	managed, err := s.resourceIsManaged(r.Context(), "user", username)
	if err != nil {
		return userResponse{}, err
	}
	return userResponse{
		Username:  user.Username,
		Admin:     user.Admin,
		Managed:   managed,
		CreatedAt: user.CreatedAt,
		Roles:     roles,
	}, nil
}

func (s *Server) handleUserRoles(
	w http.ResponseWriter,
	r *http.Request,
	username string,
) {
	if _, err := s.accountStore().User(r.Context(), username); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		roles, err := s.accountStore().UserRoles(r.Context(), username)
		if roles == nil {
			roles = []string{}
		}
		httpx.WriteResult(w, map[string]any{"username": username, "roles": roles}, err)
	case http.MethodPut:
		var request userRolesRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if request.Roles == nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", "roles is required and must be an array")
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.validateRoles(r, *request.Roles); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.accounts.SetUserRoles(r.Context(), username, *request.Roles, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"username": username, "roles": *request.Roles})
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}
