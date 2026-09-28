package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// WebhookStore is the backend capability the webhook service consumes: the
// atomic subscription mutations that fold the ownership record into one
// transaction serialized on the webhook key. The SQL store satisfies it.
type WebhookStore interface {
	SaveWebhook(context.Context, store.WebhookSave) error
	DeleteWebhook(context.Context, string, store.Ownership) error
}

// SaveWebhookCommand creates or updates an outbound webhook subscription. An
// empty Secret keeps the existing one on update.
type SaveWebhookCommand struct {
	Webhook domain.Webhook
	Create  bool
	Intent  Intent
}

// WebhookService applies webhook mutations and their ownership effects
// atomically. It is stateless beyond its backend.
type WebhookService struct {
	backend WebhookStore
}

// NewWebhookService composes the webhook command service over its backend.
func NewWebhookService(backend WebhookStore) *WebhookService {
	return &WebhookService{backend: backend}
}

// SaveWebhook applies a webhook mutation and its ownership effect atomically.
func (s *WebhookService) SaveWebhook(ctx context.Context, cmd SaveWebhookCommand) error {
	return s.backend.SaveWebhook(ctx, store.WebhookSave{
		Webhook:   cmd.Webhook,
		Create:    cmd.Create,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteWebhook removes a webhook and its ownership record atomically.
func (s *WebhookService) DeleteWebhook(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteWebhook(ctx, name, intent.ownership())
}
