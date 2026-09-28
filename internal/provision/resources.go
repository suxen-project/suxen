package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/trustmaterial"
)

type provisionedUser struct {
	Admin bool     `json:"admin"`
	Roles []string `json:"roles"`
}

type blobStoreSpec struct {
	Driver           string                         `json:"driver"`
	ConfigurationRef *domain.ConfigurationReference `json:"configurationRef"`
	Attributes       map[string]any                 `json:"attributes"`
}

type repositorySpec struct {
	Format         string                      `json:"format"`
	Type           string                      `json:"type"`
	BlobStore      string                      `json:"blobStore"`
	Members        []string                    `json:"members"`
	FormatConfig   map[string]any              `json:"formatConfig"`
	AllowOverwrite *bool                       `json:"allowOverwrite"`
	Endpoints      *domain.RepositoryEndpoints `json:"endpoints"`
}

type roleSpec struct {
	Description   string   `json:"description"`
	Privileges    []string `json:"privileges"`
	IncludedRoles []string `json:"includedRoles"`
}

type oidcProviderSpec struct {
	Issuer             string              `json:"issuer"`
	ClientID           string              `json:"clientId"`
	Scopes             []string            `json:"scopes"`
	GroupsClaim        string              `json:"groupsClaim"`
	DefaultRoles       []string            `json:"defaultRoles"`
	GroupRoles         map[string][]string `json:"groupRoles"`
	AllowPasswordGrant bool                `json:"allowPasswordGrant"`
}

type cleanupPolicySpec struct {
	Repositories []string               `json:"repositories"`
	Criteria     domain.CleanupCriteria `json:"criteria"`
	KeepLast     int                    `json:"keepLast"`
	Action       string                 `json:"action"`
	Enabled      bool                   `json:"enabled"`
}

type classificationSpec struct {
	Rules []domain.ClassificationRule `json:"rules"`
}

type trustPolicySpec struct {
	Mode                   string                 `json:"mode"`
	PublicKeys             []string               `json:"publicKeys"`
	CertificateAuthorities []string               `json:"certificateAuthorities"`
	AllowedIdentities      []domain.TrustIdentity `json:"allowedIdentities"`
	DeniedFingerprints     []string               `json:"deniedFingerprints"`
}

type downloadGateSpec struct {
	Criteria []domain.Predicate `json:"criteria"`
	Enabled  bool               `json:"enabled"`
}

type webhookSpec struct {
	URL          string   `json:"url"`
	Events       []string `json:"events"`
	Repositories []string `json:"repositories"`
	Enabled      bool     `json:"enabled"`
}

func validateResourceSpec(resource Resource) error {
	var schema any
	switch resource.Kind {
	case "blobStore":
		schema = &blobStoreSpec{}
	case "repository":
		schema = &repositorySpec{}
	case "role":
		schema = &roleSpec{}
	case "user":
		schema = &provisionedUser{}
	case "oidcProvider":
		schema = &oidcProviderSpec{}
	case "cleanupPolicy":
		schema = &cleanupPolicySpec{}
	case "classification":
		schema = &classificationSpec{}
	case "trustPolicy":
		schema = &trustPolicySpec{}
	case "downloadGate":
		schema = &downloadGateSpec{}
	case "webhook":
		schema = &webhookSpec{}
	default:
		return fmt.Errorf("unsupported kind %q", resource.Kind)
	}
	if err := requireCanonicalJSONFields(resource.Spec, reflect.TypeOf(schema).Elem()); err != nil {
		return err
	}
	encoded, err := json.Marshal(resource.Spec)
	if err != nil {
		return err
	}
	if err := decodeStrictJSON(encoded, schema); err != nil {
		return err
	}
	// Roles are flat. An empty includedRoles is tolerated for transitional
	// documents, but a nonempty value is never silently dropped.
	if role, ok := schema.(*roleSpec); ok && len(role.IncludedRoles) > 0 {
		return domain.ErrNestedRolesUnsupported
	}
	return nil
}

func (engine Engine) reconcileBlobStore(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (string, bool, error) {
	if engine.BlobStores == nil {
		return "", false, errors.New("blob-store provisioning controller is required")
	}
	current, _, err := findResource(engine.blobStoreReads().BlobStore(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	return engine.BlobStores.ReconcileBlobStore(ctx, desired, controlplane.Declarative(fingerprint), dryRun)
}

func (engine Engine) reconcileRepository(
	ctx context.Context,
	resource Resource,
	secretChanged bool,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredRepository(ctx, resource)
	if err != nil {
		return "", false, err
	}
	current, _, err := findResource(engine.repositoryReads().Repository(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	if secretChanged || (!exists && resource.Secret != "") {
		desired.Upstream = resource.Secret
	}
	if desired.BlobStore == "" {
		desired.BlobStore = "default"
	}
	if desired.Members == nil {
		desired.Members = []string{}
	}
	// The store round-trips an empty configuration as nil; normalize so
	// drift detection does not flap on {} versus absent.
	if len(desired.FormatConfig) == 0 {
		desired.FormatConfig = nil
	}
	desired.Writable = desired.Type == "hosted"
	if desired.Type == "hosted" && desired.AllowOverwrite == nil {
		allow := domain.DefaultAllowOverwrite(desired.Format)
		desired.AllowOverwrite = &allow
	}
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	// Preflight the immutable-field contract in both dry-run and apply so a
	// changed type/format is reported as a conflict rather than silently
	// recreating the resource.
	if exists && (current.Format != desired.Format || current.Type != desired.Type) {
		return "", false, invalidSpec(resource, domain.ErrImmutableRepositoryField)
	}
	// The upstream endpoint is fixed at creation; only its embedded credentials
	// may rotate. Report an endpoint change as a conflict rather than silently
	// deleting content or recreating the proxy under the same name.
	if exists {
		sameEndpoint, err := domain.SameUpstreamEndpoint(current.Upstream, desired.Upstream)
		if err != nil {
			return "", false, invalidSpec(resource, err)
		}
		if !sameEndpoint {
			return "", false, invalidSpec(resource, domain.ErrImmutableRepositoryField)
		}
	}
	if !dryRun {
		if err := engine.validateRepositoryMembers(ctx, desired); err != nil {
			return "", false, invalidSpec(resource, err)
		}
	}
	if dryRun && exists && current.BlobStore != desired.BlobStore {
		assets, err := engine.repositoryReads().Assets(ctx, resource.Name, "")
		if err != nil {
			return "", false, fmt.Errorf("check repository blob-store usage: %w", err)
		}
		if len(assets) > 0 {
			return "", false, domain.ErrRepositoryBlobStoreInUse
		}
	}
	unchanged := exists && repositoriesEqual(current, desired) && !secretChanged
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		err := engine.repositoryCommands().SaveRepository(ctx, controlplane.SaveRepositoryCommand{
			Repository: desired,
			Create:     !exists,
			Intent:     controlplane.Declarative(fingerprint),
		})
		if !exists && errors.Is(err, domain.ErrConflict) {
			// A concurrent creator (e.g. another replica) won the create race.
			// Adopt the winner through an atomic update that reconciles to the
			// desired value and writes the ownership record in one transaction,
			// rather than failing this apply or leaving the record unwritten.
			return engine.repositoryCommands().SaveRepository(ctx, controlplane.SaveRepositoryCommand{
				Repository: desired,
				Create:     false,
				Intent:     controlplane.Declarative(fingerprint),
			})
		}
		return err
	})
}

func (engine Engine) desiredRepository(
	ctx context.Context,
	resource Resource,
) (domain.Repository, bool, error) {
	current, exists, err := findResource(engine.repositoryReads().Repository(ctx, resource.Name))
	if err != nil {
		return domain.Repository{}, false, err
	}
	base := repositoryMap(current)
	if !exists {
		base = make(map[string]any)
	}
	desired := domain.Repository{}
	if err := decodeOverlay(base, resource.Spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	return desired, exists, nil
}

func (engine Engine) validateRepositoryMembers(
	ctx context.Context,
	repository domain.Repository,
) error {
	if repository.Type != "group" {
		return nil
	}
	for _, memberName := range repository.Members {
		if memberName == repository.Name {
			return invalidInput(errors.New("group repository cannot include itself"))
		}
		member, err := engine.repositoryReads().Repository(ctx, memberName)
		if err != nil {
			memberErr := fmt.Errorf("resolve member repository %q: %w", memberName, err)
			if errors.Is(err, domain.ErrNotFound) {
				return invalidInput(memberErr)
			}
			return memberErr
		}
		if member.Type == "group" {
			return invalidInput(fmt.Errorf("%w: member %q", domain.ErrNestedGroupMember, memberName))
		}
		if member.Format != repository.Format {
			return invalidInput(fmt.Errorf(
				"member repository %q has format %q, want %q",
				memberName,
				member.Format,
				repository.Format,
			))
		}
	}
	return nil
}

func (engine Engine) reconcileRole(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredRole(ctx, resource)
	if err != nil {
		return "", false, err
	}
	current, _, err := findResource(engine.roleReads().Role(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && rolesEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		err := engine.roleCommands().SaveRole(ctx, controlplane.SaveRoleCommand{
			Role:   desired,
			Create: !exists,
			Intent: controlplane.Declarative(fingerprint),
		})
		if !exists && errors.Is(err, domain.ErrConflict) {
			// A concurrent creator (e.g. another replica provisioning the built-in
			// roles) won the create race. Adopt the winner through an atomic update
			// that reconciles to the desired value and writes the ownership record
			// in one transaction, rather than failing this apply.
			return engine.roleCommands().SaveRole(ctx, controlplane.SaveRoleCommand{
				Role:   desired,
				Create: false,
				Intent: controlplane.Declarative(fingerprint),
			})
		}
		return err
	})
}

func (engine Engine) desiredRole(
	ctx context.Context,
	resource Resource,
) (domain.Role, bool, error) {
	current, exists, err := findResource(engine.roleReads().Role(ctx, resource.Name))
	if err != nil {
		return domain.Role{}, false, err
	}
	desired := current
	spec := resource.Spec
	if _, present := spec["includedRoles"]; present {
		// Roles are flat. validateResourceSpec has already rejected a nonempty
		// includedRoles, so any value left here is the tolerated empty one;
		// drop it before the strict overlay, which decodes into a domain.Role
		// that no longer carries the field.
		spec = make(map[string]any, len(resource.Spec))
		for key, value := range resource.Spec {
			spec[key] = value
		}
		delete(spec, "includedRoles")
	}
	if err := overlaySpec(spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	if desired.Privileges == nil {
		desired.Privileges = []string{}
	}
	return desired, exists, nil
}

func (engine Engine) reconcileUser(
	ctx context.Context,
	resource Resource,
	secretChanged bool,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredUser(ctx, resource)
	if err != nil {
		return "", false, err
	}
	if !exists && resource.Secret == "" {
		return "", false, invalidSpec(resource, errors.New("password or secretRef is required when creating a user"))
	}
	current := provisionedUser{}
	if exists {
		user, err := engine.accountReads().User(ctx, resource.Name)
		if err != nil {
			return "", false, err
		}
		roles, err := engine.accountReads().UserRoles(ctx, resource.Name)
		if err != nil {
			return "", false, err
		}
		current = provisionedUser{Admin: user.Admin, Roles: roles}
	}
	unchanged := exists && reflect.DeepEqual(normalizeUser(current), normalizeUser(desired)) && !secretChanged
	// Commit the account, its roles, and its ownership record in one transaction.
	password := resource.Secret
	if exists && !secretChanged {
		password = ""
	}
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.accountCommands().SaveUser(ctx, controlplane.SaveUserCommand{
			Username: resource.Name,
			Password: password,
			Admin:    desired.Admin,
			Roles:    desired.Roles,
			Create:   !exists,
			Intent:   controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) desiredUser(
	ctx context.Context,
	resource Resource,
) (provisionedUser, bool, error) {
	user, exists, err := findResource(engine.accountReads().User(ctx, resource.Name))
	if err != nil {
		return provisionedUser{}, false, err
	}
	current := provisionedUser{}
	if exists {
		roles, err := engine.accountReads().UserRoles(ctx, resource.Name)
		if err != nil {
			return provisionedUser{}, false, err
		}
		current = provisionedUser{Admin: user.Admin, Roles: roles}
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired = normalizeUser(desired)
	return desired, exists, nil
}

func (engine Engine) reconcileOIDCProvider(
	ctx context.Context,
	resource Resource,
	secretChanged bool,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredOIDCProvider(ctx, resource, secretChanged)
	if err != nil {
		return "", false, err
	}
	current, _, err := findResource(engine.oidcReads().OIDCProvider(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && oidcProvidersEqual(current, desired) && !secretChanged
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.oidcCommands().SaveOIDCProvider(ctx, controlplane.SaveOIDCProviderCommand{
			Provider: desired,
			Create:   !exists,
			Intent:   controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) desiredOIDCProvider(
	ctx context.Context,
	resource Resource,
	includeSecret bool,
) (domain.OIDCProvider, bool, error) {
	current, exists, err := findResource(engine.oidcReads().OIDCProvider(ctx, resource.Name))
	if err != nil {
		return domain.OIDCProvider{}, false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	if includeSecret || !exists {
		desired.ClientSecret = resource.Secret
	} else {
		desired.ClientSecret = ""
	}
	normalizeOIDC(&desired)
	return desired, exists, nil
}

func (engine Engine) reconcileCleanupPolicy(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredCleanupPolicy(ctx, resource)
	if err != nil {
		return "", false, err
	}
	current, _, err := findResource(engine.cleanupPolicyReads().CleanupPolicy(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && cleanupPoliciesEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.cleanupPolicyCommands().SaveCleanupPolicy(ctx, controlplane.SaveCleanupPolicyCommand{
			Policy: desired,
			Create: !exists,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) desiredCleanupPolicy(
	ctx context.Context,
	resource Resource,
) (domain.CleanupPolicy, bool, error) {
	current, exists, err := findResource(engine.cleanupPolicyReads().CleanupPolicy(ctx, resource.Name))
	if err != nil {
		return domain.CleanupPolicy{}, false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	if desired.Action == "" {
		desired.Action = "delete"
	}
	if desired.Repositories == nil {
		desired.Repositories = []string{}
	}
	desired.Repositories = uniqueSortedStrings(desired.Repositories)
	if desired.Criteria == nil {
		desired.Criteria = domain.CleanupCriteria{}
	}
	return desired, exists, nil
}

func (engine Engine) reconcileClassification(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	current, err := engine.classificationReads().Classification(ctx, resource.Name)
	if err != nil && !(dryRun && errors.Is(err, domain.ErrNotFound)) {
		return "", false, err
	}
	if errors.Is(err, domain.ErrNotFound) {
		// The spec has no inherit field, so a provisioned classification matches
		// the imperative default of inheriting the instance-wide default.
		current = domain.ClassificationConfig{
			Repository:    resource.Name,
			Rules:         []domain.ClassificationRule{},
			InheritGlobal: true,
		}
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = resource.Name
	if desired.Rules == nil {
		desired.Rules = []domain.ClassificationRule{}
	}
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	// A classification always has an effective config (it inherits when absent), so
	// the ownership record, not row existence, distinguishes create from update.
	unchanged := classificationEqual(current, desired)
	return applyDesired(unchanged, hasRecord, hasRecord, dryRun, StatusCreated, func() error {
		return engine.classificationCommands().SaveClassification(ctx, controlplane.SaveClassificationCommand{
			Config: desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) reconcileTrustPolicy(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	current, exists, err := findResource(engine.trustPolicyReads().TrustPolicy(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = resource.Name
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	if err := trustmaterial.Validate(desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && trustPoliciesEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.trustPolicyCommands().SaveTrustPolicy(ctx, controlplane.SaveTrustPolicyCommand{
			Policy: desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) reconcileDownloadGate(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	current, exists, err := findResource(engine.downloadGateReads().DownloadGate(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	desired := current
	if !exists {
		// The spec has no inherit field, so a newly provisioned gate matches the
		// imperative default of inheriting the instance-wide gate.
		desired.InheritGlobal = true
	}
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = resource.Name
	if err := desired.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && downloadGatesEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.downloadGateCommands().SaveDownloadGate(ctx, controlplane.SaveDownloadGateCommand{
			Gate:   desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

// requireDefaultsName guards the singleton default reconcilers: the instance-wide
// default of a classification, trustPolicy, or downloadGate resource is addressed
// by the reserved domain.InstanceDefaultsName, which the dispatcher routes here.
// The check keeps the reconciler correct if it is ever called directly.
func requireDefaultsName(resource Resource) error {
	if resource.Name != domain.InstanceDefaultsName {
		return invalidSpec(resource, fmt.Errorf(
			"%s instance default must be named %q", resource.Kind, domain.InstanceDefaultsName,
		))
	}
	return nil
}

func (engine Engine) reconcileDownloadGateDefaults(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	if err := requireDefaultsName(resource); err != nil {
		return "", false, err
	}
	current, exists, err := findResource(engine.downloadGateReads().DownloadGateDefaults(ctx))
	if err != nil {
		return "", false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = ""
	desired.InheritGlobal = false
	desired.Managed = true
	if err := desired.ValidateDefaults(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && downloadGateDefaultsEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUpdated, func() error {
		return engine.downloadGateCommands().SaveDownloadGateDefaults(ctx, controlplane.SaveDownloadGateDefaultsCommand{
			Gate:   desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) reconcileClassificationDefaults(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	if err := requireDefaultsName(resource); err != nil {
		return "", false, err
	}
	current, exists, err := findResource(engine.classificationReads().ClassificationDefaults(ctx))
	if err != nil {
		return "", false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = ""
	desired.InheritGlobal = false
	desired.Managed = true
	if desired.Rules == nil {
		desired.Rules = []domain.ClassificationRule{}
	}
	if err := desired.ValidateDefaults(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && classificationDefaultsEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUpdated, func() error {
		return engine.classificationCommands().SaveClassificationDefaults(ctx, controlplane.SaveClassificationDefaultsCommand{
			Config: desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) reconcileTrustPolicyDefaults(
	ctx context.Context,
	resource Resource,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	if err := requireDefaultsName(resource); err != nil {
		return "", false, err
	}
	current, exists, err := findResource(engine.trustPolicyReads().TrustPolicyDefaults(ctx))
	if err != nil {
		return "", false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	desired.Repository = ""
	desired.Managed = true
	if err := desired.ValidateDefaults(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	if err := trustmaterial.Validate(desired); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && trustPolicyDefaultsEqual(current, desired)
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUpdated, func() error {
		return engine.trustPolicyCommands().SaveTrustPolicyDefaults(ctx, controlplane.SaveTrustPolicyDefaultsCommand{
			Policy: desired,
			Intent: controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) reconcileWebhook(
	ctx context.Context,
	resource Resource,
	secretChanged bool,
	fingerprint string,
	hasRecord bool,
	dryRun bool,
) (status string, ownershipPersisted bool, err error) {
	desired, exists, err := engine.desiredWebhook(ctx, resource, secretChanged)
	if err != nil {
		return "", false, err
	}
	if !exists && resource.Secret == "" {
		return "", false, invalidSpec(resource, errors.New("secret or secretRef is required when creating a webhook"))
	}
	current, _, err := findResource(engine.webhookReads().Webhook(ctx, resource.Name))
	if err != nil {
		return "", false, err
	}
	validation := desired
	if validation.Secret == "" {
		validation.Secret = "omitted-secret-validation-placeholder"
	}
	if err := validation.Validate(); err != nil {
		return "", false, invalidSpec(resource, err)
	}
	unchanged := exists && webhooksEqual(current, desired) && !secretChanged
	return applyDesired(unchanged, exists, hasRecord, dryRun, StatusUnchanged, func() error {
		return engine.webhookCommands().SaveWebhook(ctx, controlplane.SaveWebhookCommand{
			Webhook: desired,
			Create:  !exists,
			Intent:  controlplane.Declarative(fingerprint),
		})
	})
}

func (engine Engine) desiredWebhook(
	ctx context.Context,
	resource Resource,
	includeSecret bool,
) (domain.Webhook, bool, error) {
	current, exists, err := findResource(engine.webhookReads().Webhook(ctx, resource.Name))
	if err != nil {
		return domain.Webhook{}, false, err
	}
	desired := current
	if err := overlaySpec(resource.Spec, &desired); err != nil {
		return desired, exists, invalidSpec(resource, err)
	}
	desired.Name = resource.Name
	if includeSecret || !exists {
		desired.Secret = resource.Secret
	} else {
		desired.Secret = ""
	}
	if desired.Events == nil {
		desired.Events = []string{}
	}
	if desired.Repositories == nil {
		desired.Repositories = []string{}
	}
	return desired, exists, nil
}

func mutationStatus(exists bool) string {
	if exists {
		return StatusUpdated
	}
	return StatusCreated
}

// applyDesired finalizes a migrated kind's reconcile once desired state and the
// equality verdict are known. A managed resource whose value is unchanged needs
// no write. An unmanaged but equal resource is adopted by re-saving desired
// through save, which locks the resource key and writes the desired value and the
// ownership record in one transaction; this closes the window in which an
// intervening API change would otherwise leave a managed resource that differs
// from desired or an orphan ownership record. ownershipPersisted is true whenever
// the caller must skip the generic record writer.
// adoptStatus is the status a reconciler reports when it adopts an unmanaged but
// value-equal resource: a named resource reports it as unchanged, while an
// instance-default reports the adoption as an update because taking ownership is
// an operator-visible change to the default.
func applyDesired(
	unchanged, exists, hasRecord, dryRun bool,
	adoptStatus string,
	save func() error,
) (status string, ownershipPersisted bool, err error) {
	if unchanged && hasRecord {
		return StatusUnchanged, true, nil
	}
	status = mutationStatus(exists)
	if unchanged {
		status = adoptStatus
	}
	if dryRun {
		// A preview never writes; startup re-applies to adopt an unowned resource.
		return status, false, nil
	}
	if err := save(); err != nil {
		return "", false, err
	}
	return status, true, nil
}

func invalidSpec(resource Resource, err error) error {
	return invalidInput(fmt.Errorf("invalid %s %q spec: %w", resource.Kind, resource.Name, err))
}

func findResource[T any](value T, err error) (T, bool, error) {
	if errors.Is(err, domain.ErrNotFound) {
		return value, false, nil
	}
	return value, err == nil, err
}

func overlaySpec(spec map[string]any, destination any) error {
	return decodeOverlay(nil, spec, destination)
}

func decodeOverlay(base map[string]any, spec map[string]any, destination any) error {
	value := reflect.ValueOf(destination)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("overlay destination must be a non-nil pointer")
	}
	if err := requireCanonicalJSONFields(spec, value.Elem().Type()); err != nil {
		return err
	}
	merged := make(map[string]any, len(base)+len(spec))
	for key, value := range base {
		merged[key] = value
	}
	if base == nil {
		encoded, err := json.Marshal(destination)
		if err != nil {
			return err
		}
		if err := decodeStrictJSON(encoded, &merged); err != nil {
			return err
		}
	}
	for key, entry := range spec {
		merged[key] = entry
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	decoded := reflect.New(value.Elem().Type())
	// Decode into a fresh value: an explicitly supplied map, slice, or object
	// replaces the old value, including keys and fields omitted by the spec.
	// Fields absent from the merged JSON retain their original representation,
	// including non-nil empty values omitted by omitempty and internal fields.
	copyUnspecifiedJSONFields(decoded.Elem(), value.Elem(), merged)
	if err := decodeStrictJSON(encoded, decoded.Interface()); err != nil {
		return err
	}
	value.Elem().Set(decoded.Elem())
	return nil
}

func requireCanonicalJSONFields(spec map[string]any, typ reflect.Type) error {
	if typ.Kind() != reflect.Struct {
		return nil
	}
	for key := range spec {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.SplitN(typ.Field(i).Tag.Get("json"), ",", 2)[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = typ.Field(i).Name
			}
			if strings.EqualFold(name, key) && name != key {
				return fmt.Errorf("field %q must be spelled %q", key, name)
			}
		}
	}
	return nil
}

func copyUnspecifiedJSONFields(destination, source reflect.Value, merged map[string]any) {
	if source.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < source.NumField(); i++ {
		field := source.Type().Field(i)
		name := strings.SplitN(field.Tag.Get("json"), ",", 2)[0]
		if name == "" {
			name = field.Name
		}
		specified := false
		if name != "-" {
			for key := range merged {
				if strings.EqualFold(key, name) {
					specified = true
					break
				}
			}
		}
		if !specified && destination.Field(i).CanSet() {
			destination.Field(i).Set(detachOverlayValue(source.Field(i)))
		}
	}
}

func detachOverlayValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type().Elem())
		copy.Elem().Set(detachOverlayValue(value.Elem()))
		return copy
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type()).Elem()
		copy.Set(detachOverlayValue(value.Elem()))
		return copy
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			copy.Index(i).Set(detachOverlayValue(value.Index(i)))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			copy.SetMapIndex(iter.Key(), detachOverlayValue(iter.Value()))
		}
		return copy
	case reflect.Array:
		copy := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			copy.Index(i).Set(detachOverlayValue(value.Index(i)))
		}
		return copy
	case reflect.Struct:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if copy.Field(i).CanSet() {
				copy.Field(i).Set(detachOverlayValue(value.Field(i)))
			}
		}
		return copy
	default:
		return value
	}
}

func repositoryMap(repository domain.Repository) map[string]any {
	repositoryValues := map[string]any{
		"format":    repository.Format,
		"type":      repository.Type,
		"blobStore": repository.BlobStore,
		"upstream":  repository.Upstream,
		"members":   repository.Members,
		"writable":  repository.Writable,
	}
	if repository.AllowOverwrite != nil {
		repositoryValues["allowOverwrite"] = *repository.AllowOverwrite
	}
	if len(repository.FormatConfig) != 0 {
		repositoryValues["formatConfig"] = repository.FormatConfig
	}
	if !repository.Endpoints.Empty() {
		repositoryValues["endpoints"] = repository.Endpoints
	}
	return repositoryValues
}

func normalizeUser(user provisionedUser) provisionedUser {
	if user.Roles == nil {
		user.Roles = []string{}
	}
	sort.Strings(user.Roles)
	return user
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeOIDC(provider *domain.OIDCProvider) {
	if len(provider.Scopes) == 0 {
		provider.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if provider.GroupsClaim == "" {
		provider.GroupsClaim = "groups"
	}
	if provider.DefaultRoles == nil {
		provider.DefaultRoles = []string{}
	}
	if provider.GroupRoles == nil {
		provider.GroupRoles = map[string][]string{}
	}
}

func repositoriesEqual(left domain.Repository, right domain.Repository) bool {
	// The internal id is assigned by the store, never declared in the desired
	// spec, so it must not count as drift — otherwise a concurrent-create
	// readback (which carries the winner's id) never matches the id-less desired
	// and steady-state re-apply always reports an update.
	left.ID = ""
	right.ID = ""
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}

func rolesEqual(left domain.Role, right domain.Role) bool {
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	sort.Strings(left.Privileges)
	sort.Strings(right.Privileges)
	return reflect.DeepEqual(left, right)
}

func oidcProvidersEqual(left domain.OIDCProvider, right domain.OIDCProvider) bool {
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	left.ClientSecret = ""
	right.ClientSecret = ""
	normalizeOIDC(&left)
	normalizeOIDC(&right)
	return reflect.DeepEqual(left, right)
}

func cleanupPoliciesEqual(left domain.CleanupPolicy, right domain.CleanupPolicy) bool {
	left.CreatedAt = time.Time{}
	left.UpdatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}

func classificationEqual(left domain.ClassificationConfig, right domain.ClassificationConfig) bool {
	left.UpdatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	left.Rules = normalizeClassificationRules(left.Rules)
	right.Rules = normalizeClassificationRules(right.Rules)
	return reflect.DeepEqual(left, right)
}

func normalizeClassificationRules(rules []domain.ClassificationRule) []domain.ClassificationRule {
	copy := append([]domain.ClassificationRule(nil), rules...)
	for i := range copy {
		if len(copy[i].When) == 0 {
			copy[i].When = nil
		}
	}
	return copy
}

func trustPoliciesEqual(left domain.TrustPolicy, right domain.TrustPolicy) bool {
	left.UpdatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}

func downloadGatesEqual(left domain.DownloadGate, right domain.DownloadGate) bool {
	left.UpdatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}

// The *DefaultsEqual helpers compare only the content of an instance-wide
// default: identity is fixed (singleton, no repository/inherit) and Managed is
// set by reconcile itself, so both are excluded from drift detection.
func downloadGateDefaultsEqual(left domain.DownloadGate, right domain.DownloadGate) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	left.Managed, right.Managed = false, false
	return reflect.DeepEqual(left, right)
}

func classificationDefaultsEqual(left domain.ClassificationConfig, right domain.ClassificationConfig) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	left.Managed, right.Managed = false, false
	left.Rules = normalizeClassificationRules(left.Rules)
	right.Rules = normalizeClassificationRules(right.Rules)
	return reflect.DeepEqual(left, right)
}

func trustPolicyDefaultsEqual(left domain.TrustPolicy, right domain.TrustPolicy) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	left.Managed, right.Managed = false, false
	return reflect.DeepEqual(left, right)
}

func webhooksEqual(left domain.Webhook, right domain.Webhook) bool {
	left.Secret = ""
	left.CreatedAt = time.Time{}
	left.UpdatedAt = time.Time{}
	right.Secret = ""
	right.CreatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}
