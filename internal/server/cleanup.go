package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/predicate"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// companionAssetDeleter narrows the metadata store to removing an artifact
// together with its declared companion metadata records atomically, deleting the
// artifact only while its stored row is unchanged and reporting false without
// deleting when it has changed. Retention cleanup relies on that compare so a
// candidate selected earlier is not clobbered if a concurrent write replaced it.
// It reads s.metadata on each call so a reconfigured backend is honoured.
type companionAssetDeleter interface {
	DeleteAssetWithCompanions(context.Context, domain.Asset, []string) (bool, error)
}

func (s *Server) companionAssetDeleter() companionAssetDeleter {
	return s.metadata
}

// manifestAliasCleanup narrows the metadata store to pruning the canonical
// manifest aliases that removing a set of OCI manifest assets leaves dangling.
// Both policy-driven retention cleanup and an interactive asset delete run it as
// a cascade after the asset removals. It reads s.metadata on each call so a
// reconfigured backend is honoured.
type manifestAliasCleanup interface {
	DeleteDanglingManifestAliases(context.Context, string, []domain.Asset) (int64, error)
}

func (s *Server) manifestAliasCleanup() manifestAliasCleanup {
	return s.metadata
}

type cleanupResult struct {
	Policy         string   `json:"policy"`
	Repository     string   `json:"repository"`
	DryRun         bool     `json:"dryRun"`
	Scanned        int      `json:"scanned"`
	Matched        int      `json:"matched"`
	Deleted        int      `json:"deleted"`
	Cascaded       int64    `json:"cascadedAliases"`
	SkippedChanged int      `json:"skippedChanged"`
	WouldDelete    []string `json:"wouldDelete,omitempty"`
}

type classificationRequest struct {
	Rules         []domain.ClassificationRule `json:"rules"`
	InheritGlobal *bool                       `json:"inheritGlobal,omitempty"`
}

func (request classificationRequest) domainConfig(repositoryName string) domain.ClassificationConfig {
	inheritGlobal := true
	if request.InheritGlobal != nil {
		inheritGlobal = *request.InheritGlobal
	}
	return domain.ClassificationConfig{
		Repository:    repositoryName,
		Rules:         request.Rules,
		InheritGlobal: inheritGlobal,
	}
}

func (request classificationRequest) domainDefaults() domain.ClassificationConfig {
	return domain.ClassificationConfig{Rules: request.Rules}
}

type cleanupPolicyRequest struct {
	Name         string                 `json:"name"`
	Repositories []string               `json:"repositories"`
	Criteria     domain.CleanupCriteria `json:"criteria"`
	KeepLast     int                    `json:"keepLast,omitempty"`
	Order        string                 `json:"order,omitempty"`
	Action       string                 `json:"action,omitempty"`
	Enabled      bool                   `json:"enabled"`
}

type cleanupPolicyUpdateRequest struct {
	Repositories []string               `json:"repositories"`
	Criteria     domain.CleanupCriteria `json:"criteria"`
	KeepLast     int                    `json:"keepLast,omitempty"`
	Order        string                 `json:"order,omitempty"`
	Action       string                 `json:"action,omitempty"`
	Enabled      bool                   `json:"enabled"`
}

func (request cleanupPolicyUpdateRequest) domainPolicy(name string) domain.CleanupPolicy {
	return cleanupPolicyRequest{
		Repositories: request.Repositories,
		Criteria:     request.Criteria,
		KeepLast:     request.KeepLast,
		Order:        request.Order,
		Action:       request.Action,
		Enabled:      request.Enabled,
	}.domainPolicy(name)
}

func (request cleanupPolicyRequest) domainPolicy(name string) domain.CleanupPolicy {
	if name == "" {
		name = request.Name
	}
	return domain.CleanupPolicy{
		Name:         name,
		Repositories: request.Repositories,
		Criteria:     request.Criteria,
		KeepLast:     request.KeepLast,
		Order:        request.Order,
		Action:       request.Action,
		Enabled:      request.Enabled,
	}
}

func (s *Server) handleClassification(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if !s.requireRepositoryResource(w, r, repositoryName) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		config, err := s.classificationReads().Classification(r.Context(), repositoryName)
		if err == nil {
			config.Managed, err = s.resourceIsManaged(
				r.Context(), "classification", repositoryName,
			)
		}
		httpx.WriteResult(w, config, err)
	case http.MethodPut:
		var request classificationRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		if !s.rejectManagedMutation(w, r, "classification", repositoryName) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		config := request.domainConfig(repositoryName)
		if err := s.classifications.SaveClassification(r.Context(), controlplane.SaveClassificationCommand{
			Config: config,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		stored, err := s.classificationReads().Classification(r.Context(), repositoryName)
		httpx.WriteResult(w, stored, err)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.classifications.DeleteClassification(
			r.Context(), repositoryName, controlplane.Imperative(force),
		); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// cleanupPolicyCommands is the cleanup-policy mutation capability the policy
// handlers consume from the control plane: create/update and delete, each
// folding the ownership record into one transaction.
type cleanupPolicyCommands interface {
	SaveCleanupPolicy(context.Context, controlplane.SaveCleanupPolicyCommand) error
	DeleteCleanupPolicy(context.Context, string, controlplane.Intent) error
}

// cleanupPolicyReads is the cleanup-policy read capability the cleanup handlers
// and the cleanup job need: one policy by name and the full list.
type cleanupPolicyReads interface {
	CleanupPolicy(context.Context, string) (domain.CleanupPolicy, error)
	CleanupPolicies(context.Context) ([]domain.CleanupPolicy, error)
}

// cleanupPolicyReads narrows the metadata store to the cleanup-policy read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) cleanupPolicyReads() cleanupPolicyReads {
	return s.metadata
}

func (s *Server) handleCleanupPolicyCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		policies, err := s.cleanupPolicyReads().CleanupPolicies(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		managed, err := s.managedResourceNames(r.Context(), "cleanupPolicy")
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		for index := range policies {
			policies[index].Managed = managedName(managed, policies[index].Name)
		}
		httpx.WriteCollection(w, r, "cleanup-policies", policies, func(policy domain.CleanupPolicy) string {
			return policy.Name
		})
	case http.MethodPost:
		var request cleanupPolicyRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		policy := request.domainPolicy("")
		if err := s.cleanupPolicies.SaveCleanupPolicy(r.Context(), controlplane.SaveCleanupPolicyCommand{
			Policy: policy,
			Create: true,
			Intent: controlplane.Imperative(false),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		created, err := s.cleanupPolicyReads().CleanupPolicy(r.Context(), policy.Name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(w, collectionItemLocation(r, created.Name), created)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleCleanupPolicyItem(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	switch r.Method {
	case http.MethodGet:
		policy, err := s.cleanupPolicyReads().CleanupPolicy(r.Context(), name)
		if err == nil {
			policy.Managed, err = s.resourceIsManaged(r.Context(), "cleanupPolicy", name)
		}
		httpx.WriteResult(w, policy, err)
	case http.MethodPut:
		var request cleanupPolicyUpdateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		policy := request.domainPolicy(name)
		_, err = s.cleanupPolicyReads().CleanupPolicy(r.Context(), name)
		creating := errors.Is(err, domain.ErrNotFound)
		if err != nil && !creating {
			httpx.WriteResult(w, nil, err)
			return
		}
		if err := s.cleanupPolicies.SaveCleanupPolicy(r.Context(), controlplane.SaveCleanupPolicyCommand{
			Policy: policy,
			Create: creating,
			Intent: controlplane.Imperative(force),
		}); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		updated, err := s.cleanupPolicyReads().CleanupPolicy(r.Context(), name)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		if creating {
			httpx.WriteCreated(w, r.URL.Path, updated)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		force, err := queryBoolean(r, "force")
		if err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if err := s.cleanupPolicies.DeleteCleanupPolicy(r.Context(), name, controlplane.Imperative(force)); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleRepositoryCleanup(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}
	policyName := r.URL.Query().Get("policy")
	if policyName == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "policy_required", "policy query parameter is required")
		return
	}
	dryRun, err := strconv.ParseBool(defaultString(r.URL.Query().Get("dryRun"), "true"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_dry_run", err.Error())
		return
	}
	policy, err := s.cleanupPolicyReads().CleanupPolicy(r.Context(), policyName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if !policyAppliesToRepository(policy, repositoryName) {
		httpx.WriteProblem(
			w,
			http.StatusBadRequest,
			"policy_not_attached",
			fmt.Sprintf("cleanup policy %q is not attached to repository %q", policy.Name, repositoryName),
		)
		return
	}
	task, err := s.runCleanupTask(r.Context(), policy, repositoryName, dryRun)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"cleanup_failed",
			"cleanup task failed",
			err,
		)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, task)
}

type cleanupRunResult struct {
	Policy string        `json:"policy"`
	DryRun bool          `json:"dryRun"`
	Tasks  []domain.Task `json:"tasks"`
}

// handleCleanupPolicyRun runs a cleanup policy on every repository it is attached
// to, so an operator can trigger the sweep on demand instead of waiting for the
// scheduled interval. dryRun defaults to true so a bare call previews.
func (s *Server) handleCleanupPolicyRun(
	w http.ResponseWriter,
	r *http.Request,
	policyName string,
) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}
	dryRun, err := strconv.ParseBool(defaultString(r.URL.Query().Get("dryRun"), "true"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_dry_run", err.Error())
		return
	}
	policy, err := s.cleanupPolicyReads().CleanupPolicy(r.Context(), policyName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	tasks := make([]domain.Task, 0, len(policy.Repositories))
	for _, repositoryName := range policy.Repositories {
		task, err := s.runCleanupTask(r.Context(), policy, repositoryName, dryRun)
		if err != nil {
			httpx.WriteServerProblem(
				w,
				http.StatusInternalServerError,
				"cleanup_failed",
				"cleanup task failed",
				err,
			)
			return
		}
		tasks = append(tasks, task)
	}
	httpx.WriteJSON(w, http.StatusOK, cleanupRunResult{Policy: policy.Name, DryRun: dryRun, Tasks: tasks})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	limit, err := httpx.CollectionLimit(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	snapshotID, beforeID, err := httpx.IDPageCursor(r, "tasks")
	if err != nil {
		httpx.WriteCursorError(w, err)
		return
	}
	page, err := s.tasks().TaskPage(r.Context(), snapshotID, beforeID, limit)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.WriteIDCollectionPage(w, "tasks", page.Items, page.SnapshotID, page.HasMore, func(task domain.Task) int64 {
		return task.ID
	})
}

func (s *Server) handleTaskItem(w http.ResponseWriter, r *http.Request, idValue string) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	id, err := strconv.ParseInt(idValue, 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_id", "task ID must be positive")
		return
	}
	task, err := s.tasks().Task(r.Context(), id)
	httpx.WriteResult(w, task, err)
}

// schedulerStatus is the scheduler panel's data: the current cleanup lease (nil
// when no node holds it) plus the configured job intervals, so the UI can show
// how often each background job runs without a separate config endpoint.
type schedulerStatus struct {
	Lease     *domain.Lease      `json:"lease"`
	Intervals schedulerIntervals `json:"intervals"`
}

// schedulerIntervals reports each job's configured period as a Go duration
// string (e.g. "1h0m0s"); "0s" means the job is disabled.
type schedulerIntervals struct {
	Cleanup string `json:"cleanup"`
	GC      string `json:"gc"`
	Verify  string `json:"verify"`
	Migrate string `json:"migrate"`
}

func (s *Server) handleSchedulerLeader(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	status := schedulerStatus{
		Intervals: schedulerIntervals{
			Cleanup: s.cfg.CleanupInterval.String(),
			GC:      s.cfg.GCInterval.String(),
			Verify:  s.cfg.VerifyInterval.String(),
			Migrate: s.cfg.MigrateInterval.String(),
		},
	}
	lease, err := s.leases().Lease(r.Context(), cleanupLeaseName)
	if err == nil {
		status.Lease = &lease
	} else if !errors.Is(err, domain.ErrNotFound) {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.WriteResult(w, status, nil)
}

func (s *Server) executeScheduledCleanup(ctx context.Context) error {
	policies, err := s.cleanupPolicyReads().CleanupPolicies(ctx)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		for _, repositoryName := range policy.Repositories {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := s.runCleanupTask(ctx, policy, repositoryName, false); err != nil {
				s.log.Error(
					"run scheduled cleanup",
					"policy", policy.Name,
					"repository", repositoryName,
					"error", err,
				)
			}
		}
	}
	return nil
}

func (s *Server) runCleanupTask(
	ctx context.Context,
	policy domain.CleanupPolicy,
	repositoryName string,
	dryRun bool,
) (domain.Task, error) {
	startedAt := time.Now().UTC()
	task, err := s.tasks().CreateTask(ctx, domain.Task{
		Type:       "cleanup",
		Status:     "running",
		Policy:     policy.Name,
		Repository: repositoryName,
		DryRun:     dryRun,
		StartedAt:  &startedAt,
	})
	if err != nil {
		return task, err
	}

	result, runErr := s.cleanupRepository(ctx, policy, repositoryName, dryRun, startedAt)
	s.metrics.addCleanupDeletions(repositoryName, result.Deleted, result.Cascaded)
	completedAt := time.Now().UTC()
	task.CompletedAt = &completedAt
	if runErr != nil {
		task.Status = "failed"
		task.Error = runErr.Error()
	} else {
		task.Status = "succeeded"
		task.Result, err = resultMap(result)
		if err != nil {
			runErr = err
			task.Status = "failed"
			task.Error = err.Error()
		}
	}
	if updateErr := s.finishTask(ctx, task); updateErr != nil {
		if runErr != nil {
			return task, fmt.Errorf("%w; update task: %w", runErr, updateErr)
		}
		return task, updateErr
	}
	s.content.EnqueueWebhookEvent(ctx, domain.WebhookEvent{
		ID:         randomSecret(18),
		Type:       domain.WebhookCleanupCompleted,
		Repository: repositoryName,
		OccurredAt: completedAt,
		Details: map[string]any{
			"taskId": task.ID,
			"policy": policy.Name,
			"status": task.Status,
			"dryRun": dryRun,
			"result": task.Result,
			"error":  task.Error,
		},
	})
	return task, runErr
}

func (s *Server) cleanupRepository(
	ctx context.Context,
	policy domain.CleanupPolicy,
	repositoryName string,
	dryRun bool,
	now time.Time,
) (cleanupResult, error) {
	repository, err := s.repositoryReads().Repository(ctx, repositoryName)
	if err != nil {
		return cleanupResult{}, err
	}
	assets, err := s.metadata.ForRepository(repository).Assets(ctx, "")
	if err != nil {
		return cleanupResult{}, err
	}
	grouping := repositoryRetentionGrouping(repository)
	directoryUnits, remainingAssets := selectRetentionDirectoryUnits(policy, repository, grouping, repositoryRetentionUnitDirectory(repository), assets, now)
	units, ordinaryAssets := selectRetentionUnits(policy, repository, grouping, retentionUnitPaths(repository.Format), remainingAssets, now)
	candidates, err := selectCleanupCandidates(policy, repository, grouping, ordinaryAssets, now)
	if err != nil {
		return cleanupResult{}, err
	}
	result := cleanupResult{
		Policy:     policy.Name,
		Repository: repositoryName,
		DryRun:     dryRun,
		Scanned:    len(assets),
		Matched:    len(candidates),
	}
	for _, unit := range directoryUnits {
		result.Matched += len(unit.assets)
		if dryRun {
			for _, asset := range unit.assets {
				result.WouldDelete = append(result.WouldDelete, asset.Path)
			}
			continue
		}
		deleted, err := s.metadata.DeleteAssetsInDirectoryIfUnchanged(ctx, unit.key, unit.assets)
		if err != nil {
			return result, err
		}
		if !deleted {
			result.SkippedChanged += len(unit.assets)
			continue
		}
		result.Deleted += len(unit.assets)
		for _, asset := range unit.assets {
			s.content.EnqueueAssetEvent(ctx, domain.WebhookAssetDeleted, asset)
		}
	}
	for _, unit := range units {
		result.Matched += len(unit)
		if dryRun {
			for _, asset := range unit {
				result.WouldDelete = append(result.WouldDelete, asset.Path)
			}
			continue
		}
		deleted, err := s.metadata.DeleteAssetsIfUnchanged(ctx, unit)
		if err != nil {
			return result, err
		}
		if !deleted {
			result.SkippedChanged += len(unit)
			continue
		}
		result.Deleted += len(unit)
		for _, asset := range unit {
			s.content.EnqueueAssetEvent(ctx, domain.WebhookAssetDeleted, asset)
		}
	}
	byPath := make(map[string]domain.Asset, len(assets))
	for _, asset := range assets {
		byPath[asset.Path] = asset
	}
	for _, candidate := range candidates {
		companionPaths, ok := s.declaredCompanionPaths(repository, grouping, candidate.Path)
		if !ok {
			// Fail closed: a format that declared an invalid companion path
			// keeps its artifact rather than risk an orphaned or wrong deletion.
			s.log.Warn("skip cleanup candidate with invalid companion paths",
				"repository", repositoryName, "path", candidate.Path)
			continue
		}
		if dryRun {
			result.WouldDelete = append(result.WouldDelete, candidate.Path)
			for _, companionPath := range companionPaths {
				if metadata, found := byPath[companionPath]; found && metadata.Kind == "metadata" {
					result.WouldDelete = append(result.WouldDelete, companionPath)
				}
			}
			continue
		}
		deleted, err := s.companionAssetDeleter().DeleteAssetWithCompanions(ctx, candidate, companionPaths)
		if err != nil {
			return result, err
		}
		if deleted {
			result.Deleted++
			s.content.EnqueueAssetEvent(ctx, domain.WebhookAssetDeleted, candidate)
		} else {
			result.SkippedChanged++
		}
	}
	if !dryRun {
		result.Cascaded, err = s.manifestAliasCleanup().DeleteDanglingManifestAliases(
			ctx,
			repositoryName,
			candidates,
		)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func selectCleanupCandidates(
	policy domain.CleanupPolicy,
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
	assets []domain.Asset,
	now time.Time,
) ([]domain.Asset, error) {
	matched := make([]domain.Asset, 0)
	for _, asset := range assets {
		if !cleanupSupportsAsset(asset) {
			continue
		}
		if !assetMatchesCleanupCriteria(asset, repository, policy.Criteria, now) {
			continue
		}
		matched = append(matched, asset)
	}

	retained := newestAssetIDs(matched, policy.KeepLast, policy.Order, repository, grouping)
	candidates := make([]domain.Asset, 0, len(matched))
	for _, asset := range matched {
		if _, keep := retained[asset.ID]; !keep {
			candidates = append(candidates, asset)
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].Path < candidates[right].Path
	})
	return candidates, nil
}

func assetMatchesCleanupCriteria(
	asset domain.Asset,
	repository domain.Repository,
	criteria domain.CleanupCriteria,
	now time.Time,
) bool {
	attributes := assetattrs.Project(asset, repository)
	return predicate.MatchAll(attributes, criteria, now)
}

func newestAssetIDs(
	assets []domain.Asset,
	keepLast int,
	order string,
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
) map[int64]struct{} {
	retained := make(map[int64]struct{})
	if keepLast <= 0 {
		return retained
	}
	groups := make(map[string][]domain.Asset)
	for _, asset := range assets {
		if cleanupSupportsAsset(asset) {
			key := cleanupComponent(repository, grouping, asset)
			groups[key] = append(groups[key], asset)
		}
	}
	for _, group := range groups {
		sort.Slice(group, func(left, right int) bool {
			return rankedNewer(order, assetRank(repository, group[left]), assetRank(repository, group[right]))
		})
		limit := keepLast
		if limit > len(group) {
			limit = len(group)
		}
		for _, asset := range group[:limit] {
			retained[asset.ID] = struct{}{}
		}
	}
	return retained
}

func assetRank(repository domain.Repository, asset domain.Asset) retentionRank {
	version, hasVersion := retentionVersion(repository, asset)
	return retentionRank{version: version, hasVersion: hasVersion, updatedAt: asset.UpdatedAt}
}

func cleanupSupportsAsset(asset domain.Asset) bool {
	if asset.Kind == "raw" {
		return true
	}
	return asset.Kind == "oci-manifest" &&
		asset.Reference != "" &&
		!strings.HasPrefix(asset.Reference, "sha256:")
}

// retentionGrouping returns the registered format's optional retention-grouping
// capability, or nil when the format does not own its grouping.
func retentionGrouping(formatName string) spiformat.RetentionGrouping {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	grouping, ok := registered.(spiformat.RetentionGrouping)
	if !ok {
		return nil
	}
	return grouping
}

// cleanupComponent is the keepLast grouping key: the identity whose versions
// compete for retention. A format that owns its grouping decides the key;
// otherwise the host groups OCI manifests by their image name and everything
// else by parent directory.
func cleanupComponent(
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
	asset domain.Asset,
) string {
	if grouping != nil {
		if key, ok := grouping.RetentionGroupKey(repository.FormatView(), asset.FormatView()); ok {
			return key
		}
	}
	if asset.Kind == "oci-manifest" {
		if route, ok := ocimodel.ParseAssetPath(asset.Path); ok && route.Kind == ocimodel.ManifestRoute {
			return route.ImageName
		}
	}
	directory := path.Dir(asset.Path)
	if directory == "." {
		return ""
	}
	return directory
}

// maxCompanionPaths bounds the companion records one artifact may declare.
const maxCompanionPaths = 16

// declaredCompanionPaths returns the validated companion metadata paths a hosted
// artifact's format declares. ok is false when a declared path is invalid, so
// the caller fails closed and keeps the artifact.
func (s *Server) declaredCompanionPaths(
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
	assetPath string,
) ([]string, bool) {
	if repository.Type != "hosted" || grouping == nil {
		return nil, true
	}
	declared := grouping.CompanionPaths(repository.FormatView(), assetPath)
	if len(declared) == 0 {
		return nil, true
	}
	if len(declared) > maxCompanionPaths {
		return nil, false
	}
	seen := make(map[string]struct{}, len(declared))
	valid := make([]string, 0, len(declared))
	for _, companionPath := range declared {
		if !validCompanionPath(companionPath) {
			return nil, false
		}
		if _, duplicate := seen[companionPath]; duplicate {
			continue
		}
		seen[companionPath] = struct{}{}
		valid = append(valid, companionPath)
	}
	return valid, true
}

// validCompanionPath accepts a repository-relative path with no absolute
// prefix and no empty or dot segments, so a declaration cannot escape the
// repository or name the artifact's own directory tree ambiguously.
func validCompanionPath(companionPath string) bool {
	if companionPath == "" || len(companionPath) > 512 || strings.HasPrefix(companionPath, "/") {
		return false
	}
	for _, segment := range strings.Split(companionPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func resultMap(result any) (map[string]any, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	value := make(map[string]any)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func policyAppliesToRepository(policy domain.CleanupPolicy, repositoryName string) bool {
	for _, attached := range policy.Repositories {
		if attached == repositoryName {
			return true
		}
	}
	return false
}
