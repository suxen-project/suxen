package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

const (
	// StatusCreated reports that reconciliation created a resource.
	StatusCreated = "created"
	// StatusUpdated reports that reconciliation changed an existing resource.
	StatusUpdated = "updated"
	// StatusUnchanged reports that desired and stored resource state already match.
	StatusUnchanged = "unchanged"
	// StatusDeleted reports that explicit pruning removed a managed resource.
	StatusDeleted = "deleted"
	// StatusSkipped reports that pruning left a resource in place because its
	// ownership had been transferred away (its record was already gone).
	StatusSkipped = "skipped"
	// StatusFailed reports a resource-level reconciliation failure.
	StatusFailed = "failed"
)

// kindPriorities assigns each resource kind its provisioning phase. Every
// dependency edge points to a strictly lower phase, so a stable sort by phase
// replaces recursive dependency ordering. It also doubles as the supported-kind
// set for the document parser and the prune guard.
var kindPriorities = map[string]int{
	"blobStore":      10,
	"repository":     20,
	"role":           30,
	"user":           40,
	"oidcProvider":   40,
	"cleanupPolicy":  50,
	"classification": 50,
	"trustPolicy":    50,
	"downloadGate":   50,
	"webhook":        60,
}

// repositoryGroupPhase orders group repositories after leaf repositories (20)
// and before roles (30), so a group is always applied after its member leaves.
const repositoryGroupPhase = 25

// Options controls mutation and explicit deletion behavior.
type Options struct {
	DryRun bool
	Prune  bool
}

// Result reports one deterministic reconciliation decision without secret data.
type Result struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	cause  error
}

// Report is the complete result of one desired-state reconciliation.
type Report struct {
	DryRun  bool     `json:"dryRun"`
	Prune   bool     `json:"prune"`
	Results []Result `json:"results"`
}

// Failed reports whether any resource could not be reconciled.
func (report Report) Failed() bool {
	for _, result := range report.Results {
		if result.Status == StatusFailed {
			return true
		}
	}
	return false
}

// Failure joins the underlying causes of failed resources while retaining their
// identities. It is intended for startup diagnostics; public reports use Redacted.
func (report Report) Failure() error {
	var failures []error
	for _, result := range report.Results {
		if result.Status != StatusFailed {
			continue
		}
		cause := result.cause
		if cause == nil {
			cause = errors.New(result.Error)
		}
		failures = append(failures, fmt.Errorf("%s %q: %w", result.Kind, result.Name, cause))
	}
	return errors.Join(failures...)
}

// HasValidationFailure reports whether a failed resource has an actionable
// input or domain error. Startup must stop for those errors.
func (report Report) HasValidationFailure() bool {
	for _, result := range report.Results {
		if result.Status == StatusFailed {
			if IsValidationFailure(result.cause) {
				return true
			}
		}
	}
	return false
}

// Redacted keeps actionable validation errors while replacing infrastructure
// details in reports sent to API clients. The callback receives the full cause
// for request-correlated server logging.
func (report Report) Redacted(onInternal func(Result, error)) Report {
	report.Results = append([]Result(nil), report.Results...)
	for index := range report.Results {
		result := &report.Results[index]
		if result.Status != StatusFailed {
			continue
		}
		if detail, safe := PublicError(result.cause); safe {
			result.Error = detail
			result.cause = nil
			continue
		}
		onInternal(*result, result.cause)
		result.Error = "internal provisioning error"
		result.cause = nil
	}
	return report
}

// BlobStoreController validates and applies named blob-store resources. The
// server implementation owns driver construction and readiness checks, and folds
// the ownership record into the metadata transaction: it commits the blob-store
// row and its ownership record together, doing physical backend work (readiness
// before, remember/forget after) around that transaction.
type BlobStoreController interface {
	// ReconcileBlobStore validates and creates or updates resource. A dry run must
	// perform driver and readiness preflight without persisting the resource. It
	// reports whether the mutation also committed the ownership record
	// transactionally (ownershipPersisted); false leaves the record to the
	// generic writer.
	ReconcileBlobStore(
		ctx context.Context,
		resource domain.BlobStore,
		intent controlplane.Intent,
		dryRun bool,
	) (status string, ownershipPersisted bool, err error)
	// DeleteBlobStore removes name and its ownership record in one transaction
	// unless dryRun is true, while preserving the implementation's normal
	// reference and default-store safety checks.
	DeleteBlobStore(ctx context.Context, name string, intent controlplane.Intent, dryRun bool) error
}

// AccountCommands is the account-mutation capability the engine needs from the
// control plane: create/update with roles and delete, each folding the ownership
// record into one transaction. The reconciler and prune paths for the user kind
// call it instead of the aggregate control-plane service.
type AccountCommands interface {
	SaveUser(context.Context, controlplane.SaveUserCommand) error
	DeleteUser(context.Context, string, controlplane.Intent) error
}

// OIDCCommands is the OIDC provider-mutation capability the engine needs from the
// control plane: create/update and delete, each folding the ownership record into
// one transaction. The reconciler and prune paths for the oidcProvider kind call it.
type OIDCCommands interface {
	SaveOIDCProvider(context.Context, controlplane.SaveOIDCProviderCommand) error
	DeleteOIDCProvider(context.Context, string, controlplane.Intent) error
}

// CleanupPolicyCommands is the cleanup-policy mutation capability the engine
// needs from the control plane: create/update and delete, each folding the
// ownership record into one transaction. The reconciler and prune paths for the
// cleanupPolicy kind call it.
type CleanupPolicyCommands interface {
	SaveCleanupPolicy(context.Context, controlplane.SaveCleanupPolicyCommand) error
	DeleteCleanupPolicy(context.Context, string, controlplane.Intent) error
}

// TrustPolicyCommands is the trust-policy mutation capability the engine needs
// from the control plane: per-repository and instance-default create/replace and
// delete, each folding the ownership record into one transaction. The reconciler
// and prune paths for the trustPolicy kind call it.
type TrustPolicyCommands interface {
	SaveTrustPolicy(context.Context, controlplane.SaveTrustPolicyCommand) error
	DeleteTrustPolicy(context.Context, string, controlplane.Intent) error
	SaveTrustPolicyDefaults(context.Context, controlplane.SaveTrustPolicyDefaultsCommand) error
	DeleteTrustPolicyDefaults(context.Context, controlplane.Intent) error
}

// ClassificationCommands is the classification mutation capability the engine
// needs from the control plane: per-repository and instance-default
// create/replace and delete, each folding the ownership record into one
// transaction. The reconciler and prune paths for the classification kind call it.
type ClassificationCommands interface {
	SaveClassification(context.Context, controlplane.SaveClassificationCommand) error
	DeleteClassification(context.Context, string, controlplane.Intent) error
	SaveClassificationDefaults(context.Context, controlplane.SaveClassificationDefaultsCommand) error
	DeleteClassificationDefaults(context.Context, controlplane.Intent) error
}

// DownloadGateCommands is the download-gate mutation capability the engine needs
// from the control plane: per-repository and instance-default create/replace and
// delete, each folding the ownership record into one transaction. The reconciler
// and prune paths for the downloadGate kind call it.
type DownloadGateCommands interface {
	SaveDownloadGate(context.Context, controlplane.SaveDownloadGateCommand) error
	DeleteDownloadGate(context.Context, string, controlplane.Intent) error
	SaveDownloadGateDefaults(context.Context, controlplane.SaveDownloadGateDefaultsCommand) error
	DeleteDownloadGateDefaults(context.Context, controlplane.Intent) error
}

// WebhookCommands is the webhook mutation capability the engine needs from the
// control plane: create/update and delete, each folding the ownership record into
// one transaction. The reconciler and prune paths for the webhook kind call it.
type WebhookCommands interface {
	SaveWebhook(context.Context, controlplane.SaveWebhookCommand) error
	DeleteWebhook(context.Context, string, controlplane.Intent) error
}

// RoleCommands is the role-mutation capability the engine needs from the control
// plane: create/update and delete, each folding the ownership record into one
// transaction. The reconciler and prune paths for the role kind call it.
type RoleCommands interface {
	SaveRole(context.Context, controlplane.SaveRoleCommand) error
	DeleteRole(context.Context, string, controlplane.Intent) error
}

// RepositoryCommands is the repository-mutation capability the engine needs from
// the control plane: create/update and delete, each folding the ownership record
// into one transaction. The reconciler and prune paths for the repository kind
// call it.
type RepositoryCommands interface {
	SaveRepository(context.Context, controlplane.SaveRepositoryCommand) error
	DeleteRepository(context.Context, string, controlplane.Intent) error
}

// Engine reconciles a document using the existing control-plane store.
type Engine struct {
	Store      store.Store
	BlobStores BlobStoreController
	// Accounts handles user mutations. When nil the engine falls back to a
	// store-backed service; production wiring injects the shared one.
	Accounts AccountCommands
	// OIDC handles OIDC provider mutations. When nil the engine falls back to a
	// store-backed service; production wiring injects the shared one.
	OIDC OIDCCommands
	// Roles handles role mutations. When nil the engine falls back to a
	// store-backed service; production wiring injects the shared one.
	Roles RoleCommands
	// Repositories handles repository mutations. When nil the engine falls back to
	// a store-backed service; production wiring injects the shared one.
	Repositories RepositoryCommands
	// CleanupPolicies handles cleanup-policy mutations. When nil the engine falls
	// back to a store-backed service; production wiring injects the shared one.
	CleanupPolicies CleanupPolicyCommands
	// TrustPolicies handles trust-policy mutations. When nil the engine falls back
	// to a store-backed service; production wiring injects the shared one.
	TrustPolicies TrustPolicyCommands
	// Classifications handles classification mutations. When nil the engine falls
	// back to a store-backed service; production wiring injects the shared one.
	Classifications ClassificationCommands
	// DownloadGates handles download-gate mutations. When nil the engine falls
	// back to a store-backed service; production wiring injects the shared one.
	DownloadGates DownloadGateCommands
	// Webhooks handles webhook mutations. When nil the engine falls back to a
	// store-backed service; production wiring injects the shared one.
	Webhooks WebhookCommands
	// Records is the ownership-record capability the engine reads and writes for
	// reconciliation and prune. When nil it falls back to Store. Keeping it a
	// narrow port keeps the record side distinct from the per-kind command ports
	// that fold a resource and its record into one transaction.
	Records store.OwnershipRecords
	// The *Reads ports are the per-kind read capabilities reconcile and prune
	// need. When nil each falls back to Store; production wiring injects the
	// store so the engine and its reads share one backend.
	RepositoryReads     RepositoryReader
	RoleReads           RoleReader
	AccountReads        AccountReader
	OIDCReads           OIDCProviderReader
	CleanupPolicyReads  CleanupPolicyReader
	ClassificationReads ClassificationReader
	TrustPolicyReads    TrustPolicyReader
	DownloadGateReads   DownloadGateReader
	WebhookReads        WebhookReader
	BlobStoreReads      BlobStoreReader
	Defaults            []Resource
}

// The *Reads accessors return the injected per-kind reader port, or the store
// when none was injected (a bare Engine, notably tests).
func (engine Engine) repositoryReads() RepositoryReader {
	if engine.RepositoryReads != nil {
		return engine.RepositoryReads
	}
	return engine.Store
}

func (engine Engine) roleReads() RoleReader {
	if engine.RoleReads != nil {
		return engine.RoleReads
	}
	return engine.Store
}

func (engine Engine) accountReads() AccountReader {
	if engine.AccountReads != nil {
		return engine.AccountReads
	}
	return engine.Store
}

func (engine Engine) oidcReads() OIDCProviderReader {
	if engine.OIDCReads != nil {
		return engine.OIDCReads
	}
	return engine.Store
}

func (engine Engine) cleanupPolicyReads() CleanupPolicyReader {
	if engine.CleanupPolicyReads != nil {
		return engine.CleanupPolicyReads
	}
	return engine.Store
}

func (engine Engine) classificationReads() ClassificationReader {
	if engine.ClassificationReads != nil {
		return engine.ClassificationReads
	}
	return engine.Store
}

func (engine Engine) trustPolicyReads() TrustPolicyReader {
	if engine.TrustPolicyReads != nil {
		return engine.TrustPolicyReads
	}
	return engine.Store
}

func (engine Engine) downloadGateReads() DownloadGateReader {
	if engine.DownloadGateReads != nil {
		return engine.DownloadGateReads
	}
	return engine.Store
}

func (engine Engine) webhookReads() WebhookReader {
	if engine.WebhookReads != nil {
		return engine.WebhookReads
	}
	return engine.Store
}

func (engine Engine) blobStoreReads() BlobStoreReader {
	if engine.BlobStoreReads != nil {
		return engine.BlobStoreReads
	}
	return engine.Store
}

// records returns the injected ownership-record port, or the store when none was
// injected (a bare Engine, notably in tests).
func (engine Engine) records() store.OwnershipRecords {
	if engine.Records != nil {
		return engine.Records
	}
	return engine.Store
}

// accountCommands returns the injected account command port, or a store-backed
// service when none was injected. The fallback keeps callers that build a bare
// Engine (notably tests) working while account mutations move to an injected
// port.
func (engine Engine) accountCommands() AccountCommands {
	if engine.Accounts != nil {
		return engine.Accounts
	}
	return controlplane.NewAccountService(engine.Store)
}

// oidcCommands returns the injected OIDC command port, or a store-backed service
// when none was injected. The fallback keeps callers that build a bare Engine
// (notably tests) working while OIDC mutations move to an injected port.
func (engine Engine) oidcCommands() OIDCCommands {
	if engine.OIDC != nil {
		return engine.OIDC
	}
	return controlplane.NewOIDCService(engine.Store)
}

// roleCommands returns the injected role command port, or a store-backed service
// when none was injected. The fallback keeps callers that build a bare Engine
// (notably tests) working while role mutations move to an injected port.
func (engine Engine) roleCommands() RoleCommands {
	if engine.Roles != nil {
		return engine.Roles
	}
	return controlplane.NewRoleService(engine.Store)
}

// repositoryCommands returns the injected repository command port, or a
// store-backed service when none was injected. The fallback keeps callers that
// build a bare Engine (notably tests) working while repository mutations move to
// an injected port.
func (engine Engine) repositoryCommands() RepositoryCommands {
	if engine.Repositories != nil {
		return engine.Repositories
	}
	return controlplane.NewRepositoryService(engine.Store)
}

// cleanupPolicyCommands returns the injected cleanup-policy command port, or a
// store-backed service when none was injected. The fallback keeps callers that
// build a bare Engine (notably tests) working while cleanup-policy mutations
// move to an injected port.
func (engine Engine) cleanupPolicyCommands() CleanupPolicyCommands {
	if engine.CleanupPolicies != nil {
		return engine.CleanupPolicies
	}
	return controlplane.NewCleanupPolicyService(engine.Store)
}

// trustPolicyCommands returns the injected trust-policy command port, or a
// store-backed service when none was injected. The fallback keeps callers that
// build a bare Engine (notably tests) working while trust-policy mutations move
// to an injected port.
func (engine Engine) trustPolicyCommands() TrustPolicyCommands {
	if engine.TrustPolicies != nil {
		return engine.TrustPolicies
	}
	return controlplane.NewTrustPolicyService(engine.Store)
}

// classificationCommands returns the injected classification command port, or a
// store-backed service when none was injected. The fallback keeps callers that
// build a bare Engine (notably tests) working while classification mutations
// move to an injected port.
func (engine Engine) classificationCommands() ClassificationCommands {
	if engine.Classifications != nil {
		return engine.Classifications
	}
	return controlplane.NewClassificationService(engine.Store)
}

// downloadGateCommands returns the injected download-gate command port, or a
// store-backed service when none was injected. The fallback keeps callers that
// build a bare Engine (notably tests) working while download-gate mutations move
// to an injected port.
func (engine Engine) downloadGateCommands() DownloadGateCommands {
	if engine.DownloadGates != nil {
		return engine.DownloadGates
	}
	return controlplane.NewDownloadGateService(engine.Store)
}

// webhookCommands returns the injected webhook command port, or a store-backed
// service when none was injected. The fallback keeps callers that build a bare
// Engine (notably tests) working while webhook mutations move to an injected port.
func (engine Engine) webhookCommands() WebhookCommands {
	if engine.Webhooks != nil {
		return engine.Webhooks
	}
	return controlplane.NewWebhookService(engine.Store)
}

// Apply validates and orders the complete document before making any changes.
func (engine Engine) Apply(
	ctx context.Context,
	document Document,
	options Options,
) (Report, error) {
	if engine.Store == nil {
		return Report{}, errors.New("provisioning store is required")
	}
	resources, err := engine.mergeMissingDefaultResources(ctx, document.Resources)
	if err != nil {
		return Report{}, err
	}
	ordered, err := engine.validateAndOrder(ctx, resources)
	if err != nil {
		return Report{}, err
	}
	prune, err := engine.preparePrune(ctx, ordered, options.Prune)
	if err != nil {
		return Report{}, err
	}
	if err := engine.validateReferences(ctx, ordered, prune.keys); err != nil {
		return Report{}, err
	}

	preflight := engine.reconcileOrdered(ctx, ordered, prune, true, options)
	if options.DryRun || preflight.Failed() {
		return preflight, nil
	}
	return engine.reconcileOrdered(ctx, ordered, prune, false, options), nil
}

func (engine Engine) mergeMissingDefaultResources(
	ctx context.Context,
	desired []Resource,
) ([]Resource, error) {
	merged := make([]Resource, 0, len(engine.Defaults)+len(desired))
	desiredKeys := make(map[string]struct{}, len(desired))
	for _, resource := range desired {
		var err error
		resource.Spec, err = cloneSpec(resource.Spec)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q spec: %w", resource.Kind, resource.Name, err)
		}
		desiredKeys[resourceKey(resource.Kind, resource.Name)] = struct{}{}
		merged = append(merged, resource)
	}
	for _, resource := range engine.Defaults {
		if _, explicitlyDesired := desiredKeys[resourceKey(resource.Kind, resource.Name)]; explicitlyDesired {
			continue
		}
		exists, err := engine.defaultResourceExists(ctx, resource)
		if err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		resource.Spec, err = cloneSpec(resource.Spec)
		if err != nil {
			return nil, fmt.Errorf("invalid default %s %q spec: %w", resource.Kind, resource.Name, err)
		}
		merged = append(merged, resource)
	}
	return merged, nil
}

func (engine Engine) defaultResourceExists(ctx context.Context, resource Resource) (bool, error) {
	var err error
	switch resource.Kind {
	case "repository":
		_, err = engine.repositoryReads().Repository(ctx, resource.Name)
	case "role":
		_, err = engine.roleReads().Role(ctx, resource.Name)
	default:
		return false, fmt.Errorf("unsupported built-in resource kind %q", resource.Kind)
	}
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check built-in %s %q: %w", resource.Kind, resource.Name, err)
	}
	return true, nil
}

func (engine Engine) reconcileOrdered(
	ctx context.Context,
	ordered []Resource,
	prune prunePlan,
	dryRun bool,
	options Options,
) Report {
	report := Report{DryRun: options.DryRun, Prune: options.Prune}
	for _, resource := range ordered {
		status, applyErr := engine.reconcile(ctx, resource, dryRun)
		result := Result{Kind: resource.Kind, Name: resource.Name, Status: status}
		if applyErr != nil {
			result.Status = StatusFailed
			result.Error = applyErr.Error()
			result.cause = applyErr
		}
		report.Results = append(report.Results, result)
	}

	if options.Prune && !report.Failed() {
		report.Results = append(report.Results, engine.applyPrune(ctx, prune, dryRun)...)
	}
	return report
}

func (engine Engine) reconcile(
	ctx context.Context,
	resource Resource,
	dryRun bool,
) (string, error) {
	record, hasRecord, err := engine.provisionRecord(ctx, resource.Kind, resource.Name)
	if err != nil {
		return "", err
	}
	secretChanged, fingerprint, err := compareSecret(resource.Secret, record.SecretFingerprint)
	if err != nil {
		return "", err
	}
	if !hasRecord && resource.Secret == "" {
		fingerprint = ""
	}
	// Finalize the fingerprint before the mutation so a kind that persists its
	// ownership record inside its own transaction receives the same value the
	// generic writer below would have stored.
	if resource.Secret == "" && hasRecord {
		fingerprint = record.SecretFingerprint
	}

	status, ownershipPersisted, err := engine.reconcileResource(ctx, resource, secretChanged, fingerprint, hasRecord, dryRun)
	if err != nil {
		return "", err
	}
	if dryRun {
		return status, nil
	}

	// A resource whose mutation already committed its ownership record in the
	// same transaction must not have it rewritten here.
	if ownershipPersisted {
		return status, nil
	}
	if err := engine.records().PutProvisionRecord(ctx, store.ProvisionRecord{
		Kind:              resource.Kind,
		Name:              resource.Name,
		SecretFingerprint: fingerprint,
		UpdatedAt:         time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("record provisioning state: %w", err)
	}
	return status, nil
}

// reconcileResource applies one resource and reports whether the mutation also
// committed its ownership record transactionally (ownershipPersisted). Kinds
// that return false leave the record to the generic writer in reconcile.
func (engine Engine) reconcileResource(
	ctx context.Context,
	resource Resource,
	secretChanged bool,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	if err := validateResourceSpec(resource); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	switch resource.Kind {
	case "blobStore":
		return engine.reconcileBlobStore(ctx, resource, fingerprint, hasRecord, dryRun)
	case "repository":
		return engine.reconcileRepository(ctx, resource, secretChanged, fingerprint, hasRecord, dryRun)
	case "role":
		return engine.reconcileRole(ctx, resource, fingerprint, hasRecord, dryRun)
	case "user":
		return engine.reconcileUser(ctx, resource, secretChanged, fingerprint, hasRecord, dryRun)
	case "oidcProvider":
		return engine.reconcileOIDCProvider(ctx, resource, secretChanged, fingerprint, hasRecord, dryRun)
	case "cleanupPolicy":
		return engine.reconcileCleanupPolicy(ctx, resource, fingerprint, hasRecord, dryRun)
	case "classification":
		if resource.Name == domain.InstanceDefaultsName {
			return engine.reconcileClassificationDefaults(ctx, resource, fingerprint, hasRecord, dryRun)
		}
		return engine.reconcileClassification(ctx, resource, fingerprint, hasRecord, dryRun)
	case "trustPolicy":
		if resource.Name == domain.InstanceDefaultsName {
			return engine.reconcileTrustPolicyDefaults(ctx, resource, fingerprint, hasRecord, dryRun)
		}
		return engine.reconcileTrustPolicy(ctx, resource, fingerprint, hasRecord, dryRun)
	case "downloadGate":
		if resource.Name == domain.InstanceDefaultsName {
			return engine.reconcileDownloadGateDefaults(ctx, resource, fingerprint, hasRecord, dryRun)
		}
		return engine.reconcileDownloadGate(ctx, resource, fingerprint, hasRecord, dryRun)
	case "webhook":
		return engine.reconcileWebhook(ctx, resource, secretChanged, fingerprint, hasRecord, dryRun)
	default:
		return "", false, fmt.Errorf("unsupported kind %q", resource.Kind)
	}
}

func (engine Engine) provisionRecord(
	ctx context.Context,
	kind string,
	name string,
) (store.ProvisionRecord, bool, error) {
	record, err := engine.records().ProvisionRecord(ctx, kind, name)
	if errors.Is(err, domain.ErrNotFound) {
		return store.ProvisionRecord{}, false, nil
	}
	return record, err == nil, err
}

func compareSecret(secret string, existing string) (bool, string, error) {
	if secret == "" {
		return false, existing, nil
	}
	if existing != "" {
		matches, err := secretFingerprintMatches(existing, secret)
		if err != nil {
			return false, "", err
		}
		if matches {
			return false, existing, nil
		}
	}
	fingerprint, err := newSecretFingerprint(secret)
	return true, fingerprint, err
}

func newSecretFingerprint(secret string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate secret fingerprint salt: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(nil), salt...), []byte(secret)...))
	return "v1:" + base64.RawURLEncoding.EncodeToString(salt) + ":" + hex.EncodeToString(digest[:]), nil
}

func secretFingerprintMatches(fingerprint string, secret string) (bool, error) {
	parts := strings.Split(fingerprint, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return false, errors.New("stored secret fingerprint is invalid")
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(salt) != 16 {
		return false, errors.New("stored secret fingerprint salt is invalid")
	}
	expected, err := hex.DecodeString(parts[2])
	if err != nil || len(expected) != sha256.Size {
		return false, errors.New("stored secret fingerprint digest is invalid")
	}
	actual := sha256.Sum256(append(append([]byte(nil), salt...), []byte(secret)...))
	return subtle.ConstantTimeCompare(expected, actual[:]) == 1, nil
}

func (engine Engine) validateAndOrder(
	ctx context.Context,
	resources []Resource,
) ([]Resource, error) {
	for _, resource := range resources {
		if _, unresolved := resource.Spec["__secretRef"]; unresolved {
			return nil, invalidInput(fmt.Errorf("%s %q has an unresolved secretRef", resource.Kind, resource.Name))
		}
		if resource.Secret != "" && kindSecretField(resource.Kind) == "" {
			return nil, invalidInput(fmt.Errorf("%s %q does not support secretRef", resource.Kind, resource.Name))
		}
		if err := validateResourceSpec(resource); err != nil {
			return nil, invalidSpec(resource, err)
		}
	}
	return engine.orderByPhase(ctx, resources)
}

// orderByPhase sorts resources into deterministic provisioning phases. Flat
// roles and leaf-only groups guarantee every dependency points to an earlier
// phase, so a stable sort by (phase, name, kind) replaces the recursive
// dependency walk while keeping reports order-stable across shuffled input. The
// name-before-kind tiebreak preserves the prior within-phase ordering; kind only
// breaks ties between different kinds that share a phase and a name.
func (engine Engine) orderByPhase(
	ctx context.Context,
	resources []Resource,
) ([]Resource, error) {
	type phased struct {
		resource Resource
		phase    int
	}
	candidates := make([]phased, len(resources))
	for index, resource := range resources {
		assigned, err := engine.phase(ctx, resource)
		if err != nil {
			return nil, err
		}
		candidates[index] = phased{resource: resource, phase: assigned}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].phase != candidates[right].phase {
			return candidates[left].phase < candidates[right].phase
		}
		if candidates[left].resource.Name != candidates[right].resource.Name {
			return candidates[left].resource.Name < candidates[right].resource.Name
		}
		return candidates[left].resource.Kind < candidates[right].resource.Kind
	})
	ordered := make([]Resource, len(candidates))
	for index := range candidates {
		ordered[index] = candidates[index].resource
	}
	return ordered, nil
}

// phase assigns a resource to a provisioning phase. Blob stores and flat roles
// are roots; leaf repositories follow their blob store; group repositories follow
// their leaf members; users and OIDC providers follow roles; repository-scoped
// policies and webhooks follow their repositories. Only a group repository's type
// moves it off its kind's base phase, so the effective type is decoded here.
func (engine Engine) phase(ctx context.Context, resource Resource) (int, error) {
	if resource.Kind == "repository" {
		repository, _, err := engine.desiredRepository(ctx, resource)
		if err != nil {
			return 0, err
		}
		if repository.Type == "group" {
			return repositoryGroupPhase, nil
		}
	}
	return kindPriorities[resource.Kind], nil
}

func (engine Engine) validateOIDCIssuerUniqueness(
	ctx context.Context,
	desired map[string]Resource,
	pruneKeys map[string]struct{},
) error {
	providers, err := engine.oidcReads().OIDCProviders(ctx)
	if err != nil {
		return fmt.Errorf("list OIDC providers for preflight: %w", err)
	}
	effective := make(map[string]domain.OIDCProvider, len(providers))
	for _, provider := range providers {
		if _, pruned := pruneKeys[resourceKey("oidcProvider", provider.Name)]; !pruned {
			effective[provider.Name] = provider
		}
	}
	for key, resource := range desired {
		if resourceKind(key) != "oidcProvider" {
			continue
		}
		provider, _, err := engine.desiredOIDCProvider(ctx, resource, false)
		if err != nil {
			return err
		}
		effective[provider.Name] = provider
	}
	owners := make(map[string]string, len(effective))
	for _, provider := range effective {
		if previous, duplicate := owners[provider.Issuer]; duplicate && previous != provider.Name {
			return invalidInput(fmt.Errorf(
				"OIDC providers %q and %q use the same issuer %q",
				previous,
				provider.Name,
				provider.Issuer,
			))
		}
		owners[provider.Issuer] = provider.Name
	}
	return nil
}

func (engine Engine) dependencies(ctx context.Context, resource Resource) ([]string, error) {
	switch resource.Kind {
	case "repository":
		repository, _, err := engine.desiredRepository(ctx, resource)
		if err != nil {
			return nil, err
		}
		dependencies := []string{"blobStore\x00" + defaultString(repository.BlobStore, "default")}
		if repository.Type == "group" {
			for _, member := range repository.Members {
				dependencies = append(dependencies, "repository\x00"+member)
			}
		}
		return dependencies, nil
	case "role":
		// Roles are flat: a role is a named set of privileges and depends on no
		// other role. Subjects that reference roles order themselves after them.
		return nil, nil
	case "user":
		user, _, err := engine.desiredUser(ctx, resource)
		if err != nil {
			return nil, err
		}
		dependencies := make([]string, 0, len(user.Roles))
		for _, role := range user.Roles {
			dependencies = append(dependencies, "role\x00"+role)
		}
		return dependencies, nil
	case "oidcProvider":
		provider, _, err := engine.desiredOIDCProvider(ctx, resource, false)
		if err != nil {
			return nil, err
		}
		roles := append([]string(nil), provider.DefaultRoles...)
		for _, mapped := range provider.GroupRoles {
			roles = append(roles, mapped...)
		}
		dependencies := make([]string, 0, len(roles))
		for _, role := range roles {
			dependencies = append(dependencies, "role\x00"+role)
		}
		return dependencies, nil
	case "cleanupPolicy":
		policy, _, err := engine.desiredCleanupPolicy(ctx, resource)
		if err != nil {
			return nil, err
		}
		dependencies := make([]string, 0, len(policy.Repositories))
		for _, repository := range policy.Repositories {
			dependencies = append(dependencies, "repository\x00"+repository)
		}
		return dependencies, nil
	case "classification", "trustPolicy", "downloadGate":
		if resource.Name == domain.InstanceDefaultsName {
			return nil, nil
		}
		return []string{"repository\x00" + resource.Name}, nil
	case "webhook":
		webhook, _, err := engine.desiredWebhook(ctx, resource, false)
		if err != nil {
			return nil, err
		}
		dependencies := make([]string, 0, len(webhook.Repositories))
		for _, repository := range webhook.Repositories {
			dependencies = append(dependencies, "repository\x00"+repository)
		}
		return dependencies, nil
	default:
		return nil, nil
	}
}

func (engine Engine) validateReferences(
	ctx context.Context,
	resources []Resource,
	pruneKeys map[string]struct{},
) error {
	desired := make(map[string]Resource, len(resources))
	for _, resource := range resources {
		desired[resource.Kind+"\x00"+resource.Name] = resource
	}
	if err := engine.validateOIDCIssuerUniqueness(ctx, desired, pruneKeys); err != nil {
		return err
	}
	for _, resource := range resources {
		dependencies, err := engine.dependencies(ctx, resource)
		if err != nil {
			return err
		}
		for _, dependency := range dependencies {
			if _, provided := desired[dependency]; provided {
				continue
			}
			kind, name, found := strings.Cut(dependency, "\x00")
			if !found {
				return fmt.Errorf("invalid dependency key %q", dependency)
			}
			if _, scheduledForPrune := pruneKeys[dependency]; scheduledForPrune {
				return invalidInput(fmt.Errorf(
					"%s %q references %s %q scheduled for prune",
					resource.Kind,
					resource.Name,
					kind,
					name,
				))
			}
			if err := engine.requireExistingReference(ctx, kind, name); err != nil {
				referenceErr := fmt.Errorf(
					"%s %q references missing %s %q: %w",
					resource.Kind,
					resource.Name,
					kind,
					name,
					err,
				)
				if errors.Is(err, domain.ErrNotFound) {
					return invalidInput(referenceErr)
				}
				return referenceErr
			}
		}
		if (resource.Kind == "trustPolicy" || resource.Kind == "downloadGate") &&
			resource.Name != domain.InstanceDefaultsName {
			repositoryResource, provided := desired["repository\x00"+resource.Name]
			var repository domain.Repository
			if provided {
				repository, _, err = engine.desiredRepository(ctx, repositoryResource)
			} else {
				repository, err = engine.repositoryReads().Repository(ctx, resource.Name)
			}
			if err != nil {
				return err
			}
			if repository.Type == "group" {
				if resource.Kind == "downloadGate" {
					return invalidSpec(resource, domain.ErrInvalidDownloadGate)
				}
				return invalidSpec(resource, domain.ErrInvalidTrustPolicy)
			}
		}
		if resource.Kind == "repository" {
			repository, _, err := engine.desiredRepository(ctx, resource)
			if err != nil {
				return err
			}
			if repository.Type == "group" {
				if _, err := engine.trustPolicyReads().TrustPolicy(ctx, resource.Name); err == nil {
					return invalidSpec(resource, domain.ErrInvalidTrustPolicy)
				} else if !errors.Is(err, domain.ErrNotFound) {
					return err
				}
			}
			for _, memberName := range repository.Members {
				if memberName == repository.Name {
					return invalidInput(fmt.Errorf("repository %q cannot include itself", repository.Name))
				}
				memberResource, provided := desired["repository\x00"+memberName]
				var member domain.Repository
				if provided {
					member, _, err = engine.desiredRepository(ctx, memberResource)
				} else {
					member, err = engine.repositoryReads().Repository(ctx, memberName)
				}
				if err != nil {
					memberErr := fmt.Errorf("resolve repository member %q: %w", memberName, err)
					if errors.Is(err, domain.ErrNotFound) {
						return invalidInput(memberErr)
					}
					return memberErr
				}
				if member.Format != repository.Format {
					return invalidInput(fmt.Errorf(
						"repository %q member %q has format %q, want %q",
						repository.Name,
						member.Name,
						member.Format,
						repository.Format,
					))
				}
				if member.Type == "group" {
					return invalidInput(fmt.Errorf(
						"repository %q member %q cannot itself be a group",
						repository.Name,
						member.Name,
					))
				}
			}
		}
	}
	return nil
}

func (engine Engine) requireExistingReference(
	ctx context.Context,
	kind string,
	name string,
) error {
	var err error
	switch kind {
	case "blobStore":
		_, err = engine.blobStoreReads().BlobStore(ctx, name)
	case "repository":
		_, err = engine.repositoryReads().Repository(ctx, name)
	case "role":
		_, err = engine.roleReads().Role(ctx, name)
	default:
		return fmt.Errorf("unsupported dependency kind %q", kind)
	}
	return err
}

func defaultString(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// deleteManagedResource prunes one managed resource and reports the outcome:
// StatusDeleted when the resource (and its record) were removed, or StatusSkipped
// when its ownership had been transferred away since the plan was captured, so the
// resource was left in place. Migrated kinds decide this inside their delete
// transaction; the remaining kinds re-check the ownership record first.
func (engine Engine) deleteManagedResource(
	ctx context.Context,
	record store.ProvisionRecord,
	dryRun bool,
) (string, error) {
	if dryRun && record.Kind == "blobStore" {
		if engine.BlobStores == nil {
			return "", errors.New("blob-store provisioning controller is required")
		}
		return StatusDeleted, engine.BlobStores.DeleteBlobStore(ctx, record.Name, controlplane.Declarative(""), true)
	}
	if dryRun {
		return StatusDeleted, nil
	}
	var err error
	// recordPersisted marks a kind whose delete command removes the resource and
	// its ownership record in one transaction, so the generic record delete below
	// must be skipped: repeating it can strip a record another replica created and
	// adopted for the same name in between. Such a command also returns ErrNotFound
	// when ownership was transferred away, which is a skip rather than a delete.
	recordPersisted := false
	declarative := controlplane.Declarative("")
	switch record.Kind {
	case "blobStore":
		if engine.BlobStores == nil {
			return "", errors.New("blob-store provisioning controller is required")
		}
		err, recordPersisted = engine.BlobStores.DeleteBlobStore(ctx, record.Name, declarative, false), true
	case "repository":
		err, recordPersisted = engine.repositoryCommands().DeleteRepository(ctx, record.Name, declarative), true
	case "role":
		err, recordPersisted = engine.roleCommands().DeleteRole(ctx, record.Name, declarative), true
	case "user":
		err, recordPersisted = engine.accountCommands().DeleteUser(ctx, record.Name, declarative), true
	case "oidcProvider":
		err, recordPersisted = engine.oidcCommands().DeleteOIDCProvider(ctx, record.Name, declarative), true
	case "cleanupPolicy":
		err, recordPersisted = engine.cleanupPolicyCommands().DeleteCleanupPolicy(ctx, record.Name, declarative), true
	case "classification":
		if record.Name == domain.InstanceDefaultsName {
			err, recordPersisted = engine.classificationCommands().DeleteClassificationDefaults(ctx, declarative), true
		} else {
			err, recordPersisted = engine.classificationCommands().DeleteClassification(ctx, record.Name, declarative), true
		}
	case "trustPolicy":
		if record.Name == domain.InstanceDefaultsName {
			err, recordPersisted = engine.trustPolicyCommands().DeleteTrustPolicyDefaults(ctx, declarative), true
		} else {
			err, recordPersisted = engine.trustPolicyCommands().DeleteTrustPolicy(ctx, record.Name, declarative), true
		}
	case "downloadGate":
		if record.Name == domain.InstanceDefaultsName {
			err, recordPersisted = engine.downloadGateCommands().DeleteDownloadGateDefaults(ctx, declarative), true
		} else {
			err, recordPersisted = engine.downloadGateCommands().DeleteDownloadGate(ctx, record.Name, declarative), true
		}
	case "webhook":
		err, recordPersisted = engine.webhookCommands().DeleteWebhook(ctx, record.Name, declarative), true
	default:
		return "", fmt.Errorf("unsupported managed resource kind %q", record.Kind)
	}
	if recordPersisted {
		// A migrated kind's delete owns its record's fate. ErrNotFound means its
		// ownership was transferred away, so the resource was intentionally left.
		if errors.Is(err, domain.ErrNotFound) {
			return StatusSkipped, nil
		}
		if err != nil {
			return "", fmt.Errorf("delete %s %q: %w", record.Kind, record.Name, err)
		}
		return StatusDeleted, nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return "", fmt.Errorf("delete %s %q: %w", record.Kind, record.Name, err)
	}
	if err := engine.records().DeleteProvisionRecord(ctx, record.Kind, record.Name); err != nil &&
		!errors.Is(err, domain.ErrNotFound) {
		return "", fmt.Errorf("forget provisioning state: %w", err)
	}
	return StatusDeleted, nil
}
