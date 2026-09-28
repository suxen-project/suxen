package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// OIDCStore is the backend capability the OIDC service consumes: the atomic
// provider mutations that fold the ownership record into one transaction
// serialized on the provider key. The SQL store satisfies it.
type OIDCStore interface {
	SaveOIDCProvider(context.Context, store.OIDCSave) error
	DeleteOIDCProvider(context.Context, string, store.Ownership) error
}

// SaveOIDCProviderCommand creates or updates an external identity provider. An
// empty ClientSecret keeps the existing secret on update.
type SaveOIDCProviderCommand struct {
	Provider domain.OIDCProvider
	Create   bool
	Intent   Intent
}

// OIDCService applies OIDC provider mutations and their ownership effects
// atomically. It is stateless beyond its backend and does not touch the
// verifier cache: that cache keys each verifier by provider name plus a
// fingerprint of issuer and client ID, so it rebuilds on the next verification
// whenever either changes and needs no explicit invalidation for those. The
// HTTP handlers still force an immediate rebuild after a mutation (to re-fetch
// discovery/JWKS for a same-fingerprint change); provisioning relies on the
// fingerprint comparison and does not.
type OIDCService struct {
	backend OIDCStore
}

// NewOIDCService composes the OIDC provider command service over its backend.
func NewOIDCService(backend OIDCStore) *OIDCService {
	return &OIDCService{backend: backend}
}

// SaveOIDCProvider applies a provider mutation and its ownership effect
// atomically. It returns domain.ErrManaged when an imperative caller targets a
// provisioning-managed provider without Force.
func (s *OIDCService) SaveOIDCProvider(ctx context.Context, cmd SaveOIDCProviderCommand) error {
	return s.backend.SaveOIDCProvider(ctx, store.OIDCSave{
		Provider:  cmd.Provider,
		Create:    cmd.Create,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteOIDCProvider removes a provider and its ownership record atomically.
func (s *OIDCService) DeleteOIDCProvider(ctx context.Context, name string, intent Intent) error {
	return s.backend.DeleteOIDCProvider(ctx, name, intent.ownership())
}
