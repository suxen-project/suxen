package domain

import (
	"errors"

	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// Domain errors let transports map failures to protocol-specific status codes.
//
// ErrNotFound and ErrDigestMismatch are shared with the public blob-store SPI
// so third-party drivers can return the same sentinel values the rest of the
// system matches on.
var (
	ErrNotFound                       = spiblob.ErrNotFound
	ErrConflict                       = errors.New("already exists")
	ErrManaged                        = errors.New("resource is managed by declarative provisioning")
	ErrInvalidUsername                = errors.New("username must be at most 255 bytes, start with an ASCII letter or digit, and contain only ASCII letters, digits, '.', '_', '@', '+' or '-'")
	ErrPasswordRequired               = errors.New("password is required")
	ErrReadOnly                       = errors.New("repository is read-only")
	ErrDigestMismatch                 = spiblob.ErrDigestMismatch
	ErrInvalidRepository              = errors.New("repository name must be at most 255 bytes and contain lowercase letters, digits, '-' or '_'")
	ErrInvalidAssetPath               = errors.New("asset path must contain between 1 and 2048 valid UTF-8 bytes")
	ErrReservedRepositoryName         = errors.New("repository name \"" + InstanceDefaultsName + "\" is reserved for the instance-wide policy default")
	ErrInvalidFormat                  = errors.New("repository format is not available in this build")
	ErrInvalidOverwritePolicy         = errors.New("allowOverwrite is only supported by hosted repositories")
	ErrInvalidType                    = errors.New("type must be hosted, proxy or group")
	ErrImmutableRepositoryField       = errors.New("repository type, format, and upstream endpoint cannot be changed after creation")
	ErrNestedGroupMember              = errors.New("a group member cannot itself be a group")
	ErrRepositoryInUseByGroup         = errors.New("repository is referenced by a group and cannot be deleted")
	ErrRepositoryInUseByCleanupPolicy = errors.New("repository is referenced by a cleanup policy and cannot be deleted")
	ErrRepositoryInUseByWebhook       = errors.New("repository is referenced by a webhook and cannot be deleted")
	ErrRepositoryHasUploadSessions    = errors.New("repository has upload sessions and cannot be deleted")
	ErrInvalidBlobStore               = errors.New("blob store name must be at most 255 bytes and contain lowercase letters, digits, '-' or '_'")
	ErrInvalidBlobStoreDriver         = errors.New("blob store driver must be a non-reserved name containing lowercase letters, digits, '-' or '_'")
	ErrInvalidBlobStoreConfig         = errors.New("blob store configuration must use exactly one valid environment or absolute file reference")
	ErrInvalidBlobStoreAttributes     = errors.New("blob store attributes are invalid")
	ErrBlobStoreConfigRequired        = errors.New("blob store configuration reference is required")
	ErrBlobStoreInUse                 = errors.New("blob store is referenced by a repository")
	ErrBlobStoreDefinitionImmutable   = errors.New("a blob store's driver, configuration reference and physical destination are fixed for its lifetime")
	ErrBlobStoreIdentityConflict      = errors.New("another blob store resolves to the same physical backend")
	ErrInvalidBlobStoreIdentity       = errors.New("blob store physical identity must be a SHA-256 digest")
	ErrDefaultBlobStoreImmutable      = errors.New("the default blob store cannot be updated or deleted")
	ErrBlobStoreDraining              = errors.New("a draining blob store cannot back new repositories")
	ErrBlobStoreNotDrainable          = errors.New("only an active non-default blob store can be drained")
	ErrInvalidDrainTarget             = errors.New("drain target must be a different, existing, active blob store")
	ErrActiveDrainTarget              = errors.New("blob store is the active drain target of another store")
	ErrRepositoryBlobStoreInUse       = errors.New("a repository with assets cannot change blob stores")
	ErrBlobStoreConfigChanged         = errors.New("blob store referenced configuration changed and must be reapplied")
	ErrUpstreamRequired               = errors.New("proxy repository requires an upstream")
	ErrInvalidUpstream                = errors.New("proxy repository upstream must be an http or https URL")
	ErrMembersRequired                = errors.New("group repository requires members")
	ErrOCIEndpointsNotSupported       = errors.New("endpoints are only valid on OCI repositories")
	ErrInvalidOCIEndpoint             = errors.New("OCI endpoint host or port is invalid")
	ErrOCIEndpointConflict            = errors.New("OCI endpoint host or port is already bound")
	ErrInvalidRole                    = errors.New("role name must contain lowercase letters, digits, '-' or '_'")
	ErrInvalidPrivilege               = errors.New("privilege cannot be empty")
	ErrNestedRolesUnsupported         = errors.New("nested roles are not supported; assign multiple roles to a subject instead")
	ErrRoleReferencedByProvider       = errors.New("role is referenced by an OIDC provider and cannot be deleted")
	ErrInvalidOIDCProvider            = errors.New("OIDC provider name must contain lowercase letters, digits, '-' or '_'")
	ErrInvalidOIDCIssuer              = errors.New(
		"OIDC issuer must be an http or https URL without user information, query, or fragment",
	)
	ErrOIDCClientIDRequired        = errors.New("OIDC client ID is required")
	ErrClassificationKeyRequired   = errors.New("classification rule key must be a single namespace segment")
	ErrClassificationValueRequired = errors.New("classification rule value is required")
	ErrInvalidCleanupPolicy        = errors.New("cleanup policy name must contain lowercase letters, digits, '-' or '_'")
	ErrCleanupRepositoriesRequired = errors.New("cleanup policy requires at least one repository")
	ErrCleanupCriteriaRequired     = errors.New("cleanup policy requires at least one criterion")
	ErrInvalidKeepLast             = errors.New("cleanup keepLast cannot be negative")
	ErrInvalidCleanupOrder         = errors.New("cleanup order must be updatedAt or version")
	ErrInvalidCleanupAction        = errors.New("cleanup action must be delete")
	ErrInvalidPredicatePattern     = errors.New("matches predicate must contain a valid regular expression")
	ErrInvalidPredicateDuration    = errors.New("predicate age criteria must be non-negative durations")
	ErrInvalidWebhook              = errors.New("webhook name must contain lowercase letters, digits, '-' or '_'")
	ErrInvalidWebhookURL           = errors.New("webhook URL must be an http or https URL without user information")
	ErrWebhookSecretRequired       = errors.New("webhook secret must contain at least 16 characters")
	ErrWebhookEventsRequired       = errors.New("webhook requires at least one event")
	ErrInvalidWebhookEvent         = errors.New("webhook contains an unsupported event")
	ErrInvalidDownloadGate         = errors.New("download gate requires a repository")
	ErrInvalidTrustPolicy          = errors.New("trust policy mode must be audit, verify-on-pull, or verify-on-push")
	ErrTrustMaterialRequired       = errors.New("trust policy requires a public key or certificate authority")
	ErrInvalidTrustMaterial        = errors.New("trust material is invalid")
	ErrInvalidTrustIdentity        = errors.New("trust identities require an issuer and subject")
	ErrProvenanceRejected          = errors.New("artifact provenance verification failed")
	ErrDownloadDenied              = errors.New("artifact download denied by repository policy")
	ErrUploadSessionQuotaExceeded  = errors.New("upload session quota exceeded")
	ErrUploadSessionBusy           = errors.New("upload session is busy")
	ErrUploadSessionSizeExceeded   = errors.New("upload session size exceeded")
)

var (
	ErrInvalidPredicatePath     = errors.New("predicate path is invalid")
	ErrInvalidPredicateOperator = errors.New("predicate operator is invalid")
	ErrInvalidPredicateValue    = errors.New("predicate value is invalid")
)
