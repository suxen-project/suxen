package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

type repositoryRequest struct {
	Name           string                      `json:"name"`
	Format         string                      `json:"format"`
	Type           string                      `json:"type"`
	BlobStore      string                      `json:"blobStore,omitempty"`
	Upstream       string                      `json:"upstream,omitempty"`
	Members        []string                    `json:"members,omitempty"`
	FormatConfig   map[string]any              `json:"formatConfig,omitempty"`
	AllowOverwrite *bool                       `json:"allowOverwrite,omitempty"`
	Endpoints      *domain.RepositoryEndpoints `json:"endpoints,omitempty"`
}

type repositoryUpdateRequest struct {
	Format         string                      `json:"format"`
	Type           string                      `json:"type"`
	BlobStore      string                      `json:"blobStore,omitempty"`
	Upstream       *string                     `json:"upstream,omitempty"`
	Members        []string                    `json:"members,omitempty"`
	FormatConfig   map[string]any              `json:"formatConfig,omitempty"`
	AllowOverwrite *bool                       `json:"allowOverwrite,omitempty"`
	Endpoints      *domain.RepositoryEndpoints `json:"endpoints,omitempty"`
}

func (request repositoryUpdateRequest) domainRepository(name string) domain.Repository {
	upstream := ""
	if request.Upstream != nil {
		upstream = *request.Upstream
	}
	return repositoryRequest{
		Format:         request.Format,
		Type:           request.Type,
		BlobStore:      request.BlobStore,
		Upstream:       upstream,
		Members:        request.Members,
		FormatConfig:   request.FormatConfig,
		AllowOverwrite: request.AllowOverwrite,
		Endpoints:      request.Endpoints,
	}.domainRepository(name)
}

func (request repositoryRequest) domainRepository(name string) domain.Repository {
	if name == "" {
		name = request.Name
	}
	return domain.Repository{
		Name:           name,
		Format:         request.Format,
		Type:           request.Type,
		BlobStore:      request.BlobStore,
		Upstream:       request.Upstream,
		Members:        request.Members,
		FormatConfig:   request.FormatConfig,
		AllowOverwrite: request.AllowOverwrite,
		Endpoints:      request.Endpoints,
	}
}

func (s *Server) requireRepositoryResource(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) bool {
	if _, err := s.repositoryReads().Repository(r.Context(), repositoryName); err != nil {
		httpx.WriteResult(w, nil, err)
		return false
	}
	return true
}

func (s *Server) handleRepositoryCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		repositories, err := s.readableRepositories(r)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "repository")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range repositories {
			repositories[index].Managed = managedName(managed, repositories[index].Name)
		}
		httpx.WriteCollection(w, r, "repositories", repositories, func(repository domain.Repository) string {
			return repository.Name
		})
	case http.MethodPost:
		s.createRepository(w, r)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) createRepository(w http.ResponseWriter, r *http.Request) {
	var request repositoryRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	repository := request.domainRepository("")
	if err := s.validateRepositoryRelations(r, repository); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := s.repositories.SaveRepository(r.Context(), controlplane.SaveRepositoryCommand{
		Repository: repository,
		Create:     true,
		Intent:     controlplane.Imperative(false),
	}); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	// OCI listener ports are bound at startup, so a newly configured port only
	// opens on the next restart; routing to the new repository takes effect now.

	created, err := s.repositoryReads().Repository(r.Context(), repository.Name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
}

func (s *Server) handleRepositoryItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		repository, err := s.repositoryReads().Repository(r.Context(), name)
		if err == nil {
			repository.Managed, err = s.resourceIsManaged(r.Context(), "repository", name)
		}
		httpx.WriteResult(w, repository, err)
	case http.MethodPut:
		s.updateRepository(w, r, name)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.repositories.DeleteRepository(r.Context(), name, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) updateRepository(w http.ResponseWriter, r *http.Request, name string) {
	var request repositoryUpdateRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	force, err := queryBoolean(r, "force")
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	repository := request.domainRepository(name)
	if err := s.validateRepositoryRelations(r, repository); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}

	_, err = s.repositoryReads().Repository(r.Context(), name)
	creating := errors.Is(err, domain.ErrNotFound)
	if err != nil && !creating {
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := s.repositories.SaveRepository(r.Context(), controlplane.SaveRepositoryCommand{
		Repository:       repository,
		Create:           creating,
		PreserveUpstream: request.Upstream == nil,
		Intent:           controlplane.Imperative(force),
	}); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	// OCI listener ports are bound at startup, so a changed port only opens on
	// the next restart; routing changes take effect now.
	stored, err := s.repositoryReads().Repository(r.Context(), name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if creating {
		httpx.WriteCreated(w, r.URL.Path, stored)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, stored)
}

func (s *Server) validateRepositoryRelations(
	r *http.Request,
	repository domain.Repository,
) error {
	if repository.Type != "group" {
		return nil
	}

	// Members are leaf repositories only: a group never contains a group. This
	// keeps resolution a single ordered pass over leaves and makes nesting
	// cycles impossible by construction.
	for _, memberName := range repository.Members {
		if memberName == repository.Name {
			return fmt.Errorf(
				"%w: group %q cannot contain itself",
				domain.ErrNestedGroupMember,
				repository.Name,
			)
		}
		member, err := s.repositoryReads().Repository(r.Context(), memberName)
		if err != nil {
			return fmt.Errorf("resolve group member %q: %w", memberName, err)
		}
		if member.Type == "group" {
			return fmt.Errorf(
				"%w: group member %q is a group",
				domain.ErrNestedGroupMember,
				memberName,
			)
		}
		if member.Format != repository.Format {
			return fmt.Errorf(
				"%w: group member %q uses format %q, expected %q",
				domain.ErrInvalidRepository,
				memberName,
				member.Format,
				repository.Format,
			)
		}
	}
	return nil
}

// repositoryCommands is the repository-mutation capability the HTTP handlers need
// from the control plane: create/update and delete, each folding the ownership
// record into one transaction.
type repositoryCommands interface {
	SaveRepository(context.Context, controlplane.SaveRepositoryCommand) error
	DeleteRepository(context.Context, string, controlplane.Intent) error
}

// repositoryReads is the repository read capability the handlers across the
// server need: resolve one repository by name, list all, and page the keyset.
// Repository resolution by name is the entry point; ID-scoped work that follows
// goes through store.ForRepository (a store.RepositoryView).
type repositoryReads interface {
	Repository(context.Context, string) (domain.Repository, error)
	Repositories(context.Context) ([]domain.Repository, error)
	RepositoriesPage(context.Context, string, int) (store.RepositoryKeysetPage, error)
}

// repositoryReads narrows the metadata store to the repository read capability.
// It reads s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) repositoryReads() repositoryReads {
	return s.metadata
}
