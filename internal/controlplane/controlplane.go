// Package controlplane is the internal application layer for every managed-resource
// kind: accounts, roles, repositories, blob stores, OIDC providers, cleanup
// policies, trust policies, classifications, download gates, and webhooks. The
// imperative HTTP API and declarative provisioning both call this layer, so
// ownership policy, atomic boundaries, and error classification are defined once
// rather than duplicated across those two callers.
//
// Each kind is a small feature service (AccountService, RoleService,
// RepositoryService, BlobStoreService, OIDCService, CleanupPolicyService,
// TrustPolicyService, ClassificationService, DownloadGateService, WebhookService)
// composed over a narrow, consumer-owned backend port that internal/store
// satisfies. A service owns typed commands and the ownership policy; internal/store
// owns the SQL invariants and transactions. The package deliberately does not
// import HTTP or provisioning-document types, and exposes no generic
// Apply(kind, map) command.
//
// BlobStoreService commits only the metadata half of a blob-store mutation and its
// ownership record; a blob-store mutation's physical-backend side effects (open,
// readiness, a distributed lease on delete) stay with the caller (the provisioning
// controller and the HTTP handlers), which does that work around the owned save.
package controlplane
