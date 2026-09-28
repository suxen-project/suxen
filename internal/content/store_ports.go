package content

import (
	"context"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// This file holds the narrow, consumer-owned capability ports the data plane uses
// for its OWN store access, one per concern. Each has a rt.<port>() accessor that
// narrows rt.meta() lazily (never captured), so the port stays bound to the
// current backend across SetMetadata. The Runtime field stays store.Store because
// it also distributes ports to OCI (OCIMetadata/UploadLedger) and the blob-store
// manager (BlobStoreMetadata); these ports narrow only the data plane's own code.

// WebhookOutbox is the webhook delivery-queue capability the data plane's own
// delivery worker and event enqueue need: enqueue an event, claim due deliveries
// for one worker, and record a delivery's terminal or retry state.
type WebhookOutbox interface {
	EnqueueWebhookEvent(context.Context, domain.WebhookEvent) error
	ClaimWebhookDeliveries(
		context.Context,
		string,
		time.Time,
		time.Duration,
		int,
	) ([]domain.WebhookDelivery, error)
	CompleteWebhookDelivery(
		context.Context,
		int64,
		string,
		string,
		time.Time,
		string,
	) error
}

// webhookOutbox narrows the metadata store to the webhook-outbox capability.
func (rt *Runtime) webhookOutbox() WebhookOutbox {
	return rt.meta()
}

// PolicyReader is the instance-scoped policy-read capability the data plane needs
// when it does not already hold a repository-scoped view: a repository's download
// gate by name, the instance-wide default gate, and a repository's effective
// trust policy by name. The repository-scoped equivalents go through
// store.RepositoryView (rt.metaFor); this port covers only the by-name reads.
type PolicyReader interface {
	DownloadGate(context.Context, string) (domain.DownloadGate, error)
	DownloadGateDefaults(context.Context) (domain.DownloadGate, error)
	EffectiveTrustPolicy(context.Context, string) (domain.TrustPolicy, error)
}

// policyReader narrows the metadata store to the by-name policy-read capability.
func (rt *Runtime) policyReader() PolicyReader {
	return rt.meta()
}

// RepositoryResolver resolves a repository by name. The data plane needs it to
// look up a repository it does not yet hold a scoped view for: the asset's own
// repository before a view exists, and group members named by a group. A view is
// built from the resolved repository (rt.metaFor) for the ID-scoped work that
// follows.
type RepositoryResolver interface {
	Repository(context.Context, string) (domain.Repository, error)
}

// repositoryResolver narrows the metadata store to by-name repository resolution.
func (rt *Runtime) repositoryResolver() RepositoryResolver {
	return rt.meta()
}

// BlobPlacement is the blob-placement capability the data plane needs to publish
// content: the cluster-wide write lease around a repository's store, the store a
// repository writes to, and the asset upsert that records published blobs.
type BlobPlacement interface {
	AcquireLease(context.Context, string, string, time.Time, time.Time) (bool, error)
	ReleaseLease(context.Context, string, string) error
	WriteBlobStore(context.Context, string) (string, error)
	PutAssets(context.Context, []domain.Asset) ([]domain.Asset, error)
}

// blobPlacement narrows the metadata store to the blob-placement capability.
func (rt *Runtime) blobPlacement() BlobPlacement {
	return rt.meta()
}
