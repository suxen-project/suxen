package provision

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
)

// The reader ports below are the per-kind read capabilities the reconcile and
// prune paths need from the metadata store. They are consumer-owned and split by
// kind (never one aggregate reader) so each reconciler declares exactly the reads
// it performs. Each has a matching Engine field with a store-backed fallback, so a
// bare Engine (notably tests) keeps working while production injects the store.

// RepositoryReader reads repositories for reconcile, prune reference checks, and
// the dry-run blob-store-in-use preflight (Assets).
type RepositoryReader interface {
	Repository(context.Context, string) (domain.Repository, error)
	Repositories(context.Context) ([]domain.Repository, error)
	Assets(context.Context, string, string) ([]domain.Asset, error)
}

// RoleReader reads a single role for reconcile and reference checks.
type RoleReader interface {
	Role(context.Context, string) (domain.Role, error)
}

// AccountReader reads users and their role assignments for reconcile and prune.
type AccountReader interface {
	User(context.Context, string) (domain.User, error)
	Users(context.Context) ([]domain.User, error)
	UserRoles(context.Context, string) ([]string, error)
}

// OIDCProviderReader reads OIDC providers for reconcile, issuer-uniqueness
// preflight, and prune reference checks.
type OIDCProviderReader interface {
	OIDCProvider(context.Context, string) (domain.OIDCProvider, error)
	OIDCProviders(context.Context) ([]domain.OIDCProvider, error)
}

// CleanupPolicyReader reads cleanup policies for reconcile and prune reference
// checks.
type CleanupPolicyReader interface {
	CleanupPolicy(context.Context, string) (domain.CleanupPolicy, error)
	CleanupPolicies(context.Context) ([]domain.CleanupPolicy, error)
}

// ClassificationReader reads a repository classification and the instance-wide
// default for reconcile.
type ClassificationReader interface {
	Classification(context.Context, string) (domain.ClassificationConfig, error)
	ClassificationDefaults(context.Context) (domain.ClassificationConfig, error)
}

// TrustPolicyReader reads a repository trust policy and the instance-wide default
// for reconcile and the group-repository conflict check.
type TrustPolicyReader interface {
	TrustPolicy(context.Context, string) (domain.TrustPolicy, error)
	TrustPolicyDefaults(context.Context) (domain.TrustPolicy, error)
}

// DownloadGateReader reads a repository download gate and the instance-wide
// default for reconcile.
type DownloadGateReader interface {
	DownloadGate(context.Context, string) (domain.DownloadGate, error)
	DownloadGateDefaults(context.Context) (domain.DownloadGate, error)
}

// WebhookReader reads webhooks for reconcile and prune reference checks.
type WebhookReader interface {
	Webhook(context.Context, string) (domain.Webhook, error)
	Webhooks(context.Context) ([]domain.Webhook, error)
}

// BlobStoreReader reads a single blob store for reconcile and reference checks.
type BlobStoreReader interface {
	BlobStore(context.Context, string) (domain.BlobStore, error)
}
