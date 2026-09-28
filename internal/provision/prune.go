package provision

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type prunePlan struct {
	records []store.ProvisionRecord
	keys    map[string]struct{}
}

func (engine Engine) preparePrune(
	ctx context.Context,
	desired []Resource,
	enabled bool,
) (prunePlan, error) {
	plan := prunePlan{keys: make(map[string]struct{})}
	if !enabled {
		return plan, nil
	}
	desiredKeys := make(map[string]struct{}, len(desired)+len(engine.Defaults))
	for _, resource := range desired {
		desiredKeys[resourceKey(resource.Kind, resource.Name)] = struct{}{}
	}
	// Built-ins are creation defaults rather than perpetually reconciled desired
	// state, but an explicit prune must still preserve their ownership records.
	for _, resource := range engine.Defaults {
		desiredKeys[resourceKey(resource.Kind, resource.Name)] = struct{}{}
	}
	records, err := engine.records().ProvisionRecords(ctx)
	if err != nil {
		return plan, fmt.Errorf("list provisioning records: %w", err)
	}
	for _, record := range records {
		key := resourceKey(record.Kind, record.Name)
		if _, retained := desiredKeys[key]; retained {
			continue
		}
		if _, supported := kindPriorities[record.Kind]; !supported {
			return plan, fmt.Errorf("unsupported managed resource kind %q", record.Kind)
		}
		plan.records = append(plan.records, record)
		plan.keys[key] = struct{}{}
	}
	if err := engine.validatePruneReferences(ctx, plan.keys, desired); err != nil {
		return prunePlan{}, err
	}
	ordered, err := engine.orderPrune(ctx, plan.records)
	if err != nil {
		return prunePlan{}, err
	}
	plan.records = ordered
	return plan, nil
}

func (engine Engine) validatePruneReferences(
	ctx context.Context,
	pruneKeys map[string]struct{},
	desiredResources []Resource,
) error {
	if len(pruneKeys) == 0 {
		return nil
	}
	if engine.BlobStores == nil {
		for key := range pruneKeys {
			if resourceKind(key) == "blobStore" {
				return errors.New("blob-store provisioning controller is required for prune")
			}
		}
	}
	if _, pruned := pruneKeys[resourceKey("blobStore", "default")]; pruned {
		return invalidInput(domain.ErrDefaultBlobStoreImmutable)
	}
	desired := make(map[string]Resource, len(desiredResources))
	for _, resource := range desiredResources {
		desired[resourceKey(resource.Kind, resource.Name)] = resource
	}

	repositories, err := engine.repositoryReads().Repositories(ctx)
	if err != nil {
		return fmt.Errorf("validate repository prune references: %w", err)
	}
	for _, repository := range repositories {
		if isPruned(pruneKeys, "repository", repository.Name) {
			continue
		}
		if resource, replaced := desired[resourceKey("repository", repository.Name)]; replaced {
			repository, _, err = engine.desiredRepository(ctx, resource)
			if err != nil {
				return err
			}
		}
		if isPruned(pruneKeys, "blobStore", defaultString(repository.BlobStore, "default")) {
			return invalidInput(fmt.Errorf(
				"blobStore %q cannot be pruned while repository %q uses it",
				repository.BlobStore,
				repository.Name,
			))
		}
		for _, member := range repository.Members {
			if isPruned(pruneKeys, "repository", member) {
				return invalidInput(fmt.Errorf(
					"repository %q cannot be pruned while group %q references it",
					member,
					repository.Name,
				))
			}
		}
	}

	// Roles are flat, so no role can block another role's prune. Subjects that
	// still reference a role do, and are validated below.
	users, err := engine.accountReads().Users(ctx)
	if err != nil {
		return fmt.Errorf("validate user role prune references: %w", err)
	}
	for _, user := range users {
		if isPruned(pruneKeys, "user", user.Username) {
			continue
		}
		assigned, err := engine.accountReads().UserRoles(ctx, user.Username)
		if resource, replaced := desired[resourceKey("user", user.Username)]; replaced {
			configured, _, desiredErr := engine.desiredUser(ctx, resource)
			if desiredErr != nil {
				return desiredErr
			}
			assigned = configured.Roles
		} else if err != nil {
			return fmt.Errorf("read roles for user %q: %w", user.Username, err)
		}
		for _, role := range assigned {
			if isPruned(pruneKeys, "role", role) {
				return invalidInput(fmt.Errorf(
					"role %q cannot be pruned while user %q references it",
					role,
					user.Username,
				))
			}
		}
	}
	providers, err := engine.oidcReads().OIDCProviders(ctx)
	if err != nil {
		return fmt.Errorf("validate OIDC role prune references: %w", err)
	}
	for _, provider := range providers {
		if isPruned(pruneKeys, "oidcProvider", provider.Name) {
			continue
		}
		if resource, replaced := desired[resourceKey("oidcProvider", provider.Name)]; replaced {
			provider, _, err = engine.desiredOIDCProvider(ctx, resource, false)
			if err != nil {
				return err
			}
		}
		roles := append([]string(nil), provider.DefaultRoles...)
		for _, mapped := range provider.GroupRoles {
			roles = append(roles, mapped...)
		}
		for _, role := range roles {
			if isPruned(pruneKeys, "role", role) {
				return invalidInput(fmt.Errorf(
					"role %q cannot be pruned while OIDC provider %q references it",
					role,
					provider.Name,
				))
			}
		}
	}

	policies, err := engine.cleanupPolicyReads().CleanupPolicies(ctx)
	if err != nil {
		return fmt.Errorf("validate cleanup policy prune references: %w", err)
	}
	for _, policy := range policies {
		if isPruned(pruneKeys, "cleanupPolicy", policy.Name) {
			continue
		}
		if resource, replaced := desired[resourceKey("cleanupPolicy", policy.Name)]; replaced {
			policy, _, err = engine.desiredCleanupPolicy(ctx, resource)
			if err != nil {
				return err
			}
		}
		for _, repository := range policy.Repositories {
			if isPruned(pruneKeys, "repository", repository) {
				return invalidInput(fmt.Errorf(
					"repository %q cannot be pruned while cleanup policy %q references it",
					repository,
					policy.Name,
				))
			}
		}
	}
	webhooks, err := engine.webhookReads().Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("validate webhook prune references: %w", err)
	}
	for _, webhook := range webhooks {
		if isPruned(pruneKeys, "webhook", webhook.Name) {
			continue
		}
		if resource, replaced := desired[resourceKey("webhook", webhook.Name)]; replaced {
			webhook, _, err = engine.desiredWebhook(ctx, resource, false)
			if err != nil {
				return err
			}
		}
		for _, repository := range webhook.Repositories {
			if isPruned(pruneKeys, "repository", repository) {
				return invalidInput(fmt.Errorf(
					"repository %q cannot be pruned while webhook %q references it",
					repository,
					webhook.Name,
				))
			}
		}
	}
	return nil
}

// orderPrune sorts prune records into reverse provisioning-phase order so
// dependents are deleted before the resources they reference. It is the exact
// reverse of the create ordering — descending (phase, name, kind). Reference
// safety against retained resources is enforced separately by
// validatePruneReferences; the records passed here are already exactly the set
// to delete.
func (engine Engine) orderPrune(
	ctx context.Context,
	records []store.ProvisionRecord,
) ([]store.ProvisionRecord, error) {
	type phased struct {
		record store.ProvisionRecord
		phase  int
	}
	ordered := make([]phased, len(records))
	for index, record := range records {
		assigned, err := engine.prunePhase(ctx, record)
		if err != nil {
			return nil, err
		}
		ordered[index] = phased{record: record, phase: assigned}
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].phase != ordered[right].phase {
			return ordered[left].phase > ordered[right].phase
		}
		if ordered[left].record.Name != ordered[right].record.Name {
			return ordered[left].record.Name > ordered[right].record.Name
		}
		return ordered[left].record.Kind > ordered[right].record.Kind
	})
	result := make([]store.ProvisionRecord, len(ordered))
	for index := range ordered {
		result[index] = ordered[index].record
	}
	return result, nil
}

// prunePhase assigns a stored record its provisioning phase, reading a
// repository's type so group repositories sort after their leaf members. A
// record whose resource is already gone falls back to its kind's base phase.
func (engine Engine) prunePhase(ctx context.Context, record store.ProvisionRecord) (int, error) {
	if record.Kind == "repository" {
		repository, err := engine.repositoryReads().Repository(ctx, record.Name)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return kindPriorities["repository"], nil
			}
			return 0, err
		}
		if repository.Type == "group" {
			return repositoryGroupPhase, nil
		}
	}
	return kindPriorities[record.Kind], nil
}

func (engine Engine) applyPrune(
	ctx context.Context,
	plan prunePlan,
	dryRun bool,
) []Result {
	results := make([]Result, 0, len(plan.records))
	for _, record := range plan.records {
		result := Result{Kind: record.Kind, Name: record.Name}
		status, err := engine.deleteManagedResource(ctx, record, dryRun)
		if err != nil {
			result.Status = StatusFailed
			result.Error = err.Error()
			result.cause = err
		} else {
			result.Status = status
		}
		results = append(results, result)
	}
	return results
}

func resourceKey(kind string, name string) string {
	return kind + "\x00" + name
}

func resourceKind(key string) string {
	for index := range key {
		if key[index] == 0 {
			return key[:index]
		}
	}
	return key
}

func isPruned(keys map[string]struct{}, kind string, name string) bool {
	_, found := keys[resourceKey(kind, name)]
	return found
}
