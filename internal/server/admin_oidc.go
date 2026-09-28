package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

type oidcProviderRequest struct {
	Name               string              `json:"name"`
	Issuer             string              `json:"issuer"`
	ClientID           string              `json:"clientId"`
	ClientSecret       string              `json:"clientSecret,omitempty"`
	Scopes             []string            `json:"scopes,omitempty"`
	GroupsClaim        string              `json:"groupsClaim,omitempty"`
	DefaultRoles       []string            `json:"defaultRoles,omitempty"`
	GroupRoles         map[string][]string `json:"groupRoles,omitempty"`
	AllowPasswordGrant bool                `json:"allowPasswordGrant,omitempty"`
}

type oidcProviderUpdateRequest struct {
	Issuer             string              `json:"issuer"`
	ClientID           string              `json:"clientId"`
	ClientSecret       string              `json:"clientSecret,omitempty"`
	Scopes             []string            `json:"scopes,omitempty"`
	GroupsClaim        string              `json:"groupsClaim,omitempty"`
	DefaultRoles       []string            `json:"defaultRoles,omitempty"`
	GroupRoles         map[string][]string `json:"groupRoles,omitempty"`
	AllowPasswordGrant bool                `json:"allowPasswordGrant,omitempty"`
}

func (request oidcProviderUpdateRequest) domainProvider(name string) domain.OIDCProvider {
	return oidcProviderRequest{
		Issuer:             request.Issuer,
		ClientID:           request.ClientID,
		ClientSecret:       request.ClientSecret,
		Scopes:             request.Scopes,
		GroupsClaim:        request.GroupsClaim,
		DefaultRoles:       request.DefaultRoles,
		GroupRoles:         request.GroupRoles,
		AllowPasswordGrant: request.AllowPasswordGrant,
	}.domainProvider(name)
}

func (request oidcProviderRequest) domainProvider(name string) domain.OIDCProvider {
	if name == "" {
		name = request.Name
	}
	return domain.OIDCProvider{
		Name:               name,
		Issuer:             request.Issuer,
		ClientID:           request.ClientID,
		ClientSecret:       request.ClientSecret,
		Scopes:             request.Scopes,
		GroupsClaim:        request.GroupsClaim,
		DefaultRoles:       request.DefaultRoles,
		GroupRoles:         request.GroupRoles,
		AllowPasswordGrant: request.AllowPasswordGrant,
	}
}

func (s *Server) handleOIDCProviderCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		providers, err := s.oidcReads().OIDCProviders(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "oidcProvider")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range providers {
			providers[index].Managed = managedName(managed, providers[index].Name)
		}
		httpx.WriteCollection(w, r, "oidc-providers", providers, func(provider domain.OIDCProvider) string {
			return provider.Name
		})
	case http.MethodPost:
		s.createOIDCProvider(w, r)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// oidcCommands is the OIDC provider-mutation capability the OIDC handlers consume
// from the control plane: create/update and delete, each folding the ownership
// record into one transaction. Verifier-cache invalidation stays a post-commit
// handler effect.
type oidcCommands interface {
	SaveOIDCProvider(context.Context, controlplane.SaveOIDCProviderCommand) error
	DeleteOIDCProvider(context.Context, string, controlplane.Intent) error
}

// oidcReads is the OIDC provider-read capability the OIDC handlers, the auth
// setup, and the discovery endpoint need: one provider by name and the full
// list.
type oidcReads interface {
	OIDCProvider(context.Context, string) (domain.OIDCProvider, error)
	OIDCProviders(context.Context) ([]domain.OIDCProvider, error)
}

// oidcReads narrows the metadata store to the OIDC provider-read capability. It
// reads s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) oidcReads() oidcReads {
	return s.metadata
}

func (s *Server) createOIDCProvider(w http.ResponseWriter, r *http.Request) {
	var request oidcProviderRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	provider := request.domainProvider("")
	if err := s.validateOIDCProviderRoles(r, provider); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := s.oidc.SaveOIDCProvider(r.Context(), controlplane.SaveOIDCProviderCommand{
		Provider: provider,
		Create:   true,
		Intent:   controlplane.Imperative(false),
	}); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	created, err := s.oidcReads().OIDCProvider(r.Context(), provider.Name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
}

func (s *Server) handleOIDCProviderItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		provider, err := s.oidcReads().OIDCProvider(r.Context(), name)
		if err == nil {
			provider.Managed, err = s.resourceIsManaged(r.Context(), "oidcProvider", name)
		}
		httpx.WriteResult(w, provider, err)
	case http.MethodPut:
		s.updateOIDCProvider(w, r, name)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.oidc.DeleteOIDCProvider(r.Context(), name, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		s.identity.InvalidateOIDCVerifier(name)
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) updateOIDCProvider(w http.ResponseWriter, r *http.Request, name string) {
	var request oidcProviderUpdateRequest
	if !httpx.DecodeJSON(w, r, &request) {
		return
	}
	force, err := queryBoolean(r, "force")
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	provider := request.domainProvider(name)
	_, err = s.oidcReads().OIDCProvider(r.Context(), name)
	creating := errors.Is(err, domain.ErrNotFound)
	if err != nil && !creating {
		httpx.WriteResult(w, nil, err)
		return
	}
	// An omitted secret keeps the stored one: the store leaves the column
	// untouched when ClientSecret is empty on update.
	if err := s.validateOIDCProviderRoles(r, provider); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := s.oidc.SaveOIDCProvider(r.Context(), controlplane.SaveOIDCProviderCommand{
		Provider: provider,
		Create:   creating,
		Intent:   controlplane.Imperative(force),
	}); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	s.identity.InvalidateOIDCVerifier(name)
	updated, err := s.oidcReads().OIDCProvider(r.Context(), name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if creating {
		httpx.WriteCreated(w, r.URL.Path, updated)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

func (s *Server) validateOIDCProviderRoles(
	r *http.Request,
	provider domain.OIDCProvider,
) error {
	roleNames := append([]string{}, provider.DefaultRoles...)
	for _, groupRoles := range provider.GroupRoles {
		roleNames = append(roleNames, groupRoles...)
	}
	return s.validateRoles(r, uniqueSortedStrings(roleNames))
}
