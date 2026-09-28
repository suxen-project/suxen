package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

// webhookCommands is the webhook mutation capability the webhook handlers
// consume from the control plane: create/update and delete, each folding the
// ownership record into one transaction.
type webhookCommands interface {
	SaveWebhook(context.Context, controlplane.SaveWebhookCommand) error
	DeleteWebhook(context.Context, string, controlplane.Intent) error
}

// webhookReads is the webhook read capability the webhook handlers need: one
// webhook by name, the full list, and a page of a webhook's deliveries.
type webhookReads interface {
	Webhook(context.Context, string) (domain.Webhook, error)
	Webhooks(context.Context) ([]domain.Webhook, error)
	WebhookDeliveryPage(context.Context, string, int64, int64, int) (store.IDPage[domain.WebhookDelivery], error)
}

// webhookReads narrows the metadata store to the webhook-read capability. It
// reads s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) webhookReads() webhookReads {
	return s.metadata
}

type webhookRequest struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Secret       string   `json:"secret,omitempty"`
	Events       []string `json:"events"`
	Repositories []string `json:"repositories,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
}

func (request webhookRequest) domainWebhook(name string) domain.Webhook {
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	return domain.Webhook{
		Name:         name,
		URL:          request.URL,
		Secret:       request.Secret,
		Events:       request.Events,
		Repositories: request.Repositories,
		Enabled:      enabled,
	}
}

func (s *Server) handleWebhookCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		webhooks, err := s.webhookReads().Webhooks(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "webhook")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range webhooks {
			webhooks[index].Managed = managedName(managed, webhooks[index].Name)
		}
		httpx.WriteCollection(w, r, "webhooks", webhooks, func(webhook domain.Webhook) string {
			return webhook.Name
		})
	case http.MethodPost:
		var request webhookRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		webhook := request.domainWebhook(request.Name)
		if err := s.webhooks.SaveWebhook(r.Context(), controlplane.SaveWebhookCommand{
			Webhook: webhook,
			Create:  true,
			Intent:  controlplane.Imperative(false),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		created, err := s.webhookReads().Webhook(r.Context(), webhook.Name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleWebhookItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		webhook, err := s.webhookReads().Webhook(r.Context(), name)
		if err == nil {
			webhook.Managed, err = s.resourceIsManaged(r.Context(), "webhook", name)
		}
		httpx.WriteResult(w, webhook, err)
	case http.MethodPut:
		_, existingErr := s.webhookReads().Webhook(r.Context(), name)
		creating := errors.Is(existingErr, domain.ErrNotFound)
		if existingErr != nil && !creating {
			httpx.WriteResult(w, nil, existingErr)
			return
		}
		var request webhookRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		webhook := request.domainWebhook(name)
		if err := s.webhooks.SaveWebhook(r.Context(), controlplane.SaveWebhookCommand{
			Webhook: webhook,
			Create:  false,
			Intent:  controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.webhookReads().Webhook(r.Context(), name)
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
		if err := s.webhooks.DeleteWebhook(r.Context(), name, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleWebhookDeliveries(
	w http.ResponseWriter,
	r *http.Request,
	webhookName string,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	resource := "webhook-deliveries:" + webhookName
	snapshotID, beforeID, err := httpx.IDPageCursor(r, resource)
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	page, err := s.webhookReads().WebhookDeliveryPage(
		r.Context(), webhookName, snapshotID, beforeID, limit,
	)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.WriteIDCollectionPage(
		w,
		resource,
		page.Items,
		page.SnapshotID,
		page.HasMore,
		func(delivery domain.WebhookDelivery) int64 {
			return delivery.ID
		},
	)
}
