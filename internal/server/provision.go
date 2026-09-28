package server

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/controlplane"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/startup"
	"github.com/suxen-project/suxen/internal/store"
)

// provisionRecordReads is the provision-record read capability the server's
// managed-resource reporting needs: one record by (kind, name) and the full
// list. The record writers live on the provision engine's own port.
type provisionRecordReads interface {
	ProvisionRecord(context.Context, string, string) (store.ProvisionRecord, error)
	ProvisionRecords(context.Context) ([]store.ProvisionRecord, error)
}

// provisionRecordReads narrows the metadata store to the provision-record read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) provisionRecordReads() provisionRecordReads {
	return s.metadata
}

const (
	provisionLeaseName       = "declarative-provisioning"
	provisionLeaseDuration   = 30 * time.Second
	provisionLeaseRenewal    = 10 * time.Second
	provisionLeaseRetry      = 250 * time.Millisecond
	provisionStartupDeadline = 10 * time.Minute
)

type provisionBlobStoreController struct {
	server *Server
	// commands is the metadata+ownership half of a blob-store mutation, built over
	// the same store the engine reconciles against.
	commands          blobStoreCommands
	mu                sync.Mutex
	preflightIdentity map[string]string
}

// managedResourceRecord identifies one resource owned by declarative
// provisioning without exposing internal comparison hashes.
type managedResourceRecord struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (controller *provisionBlobStoreController) ReconcileBlobStore(
	ctx context.Context,
	desired domain.BlobStore,
	intent controlplane.Intent,
	dryRun bool,
) (string, bool, error) {
	existing, err := controller.server.blobStoreReads().BlobStore(ctx, desired.Name)
	exists := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return "", false, err
	}

	prepared, configuration, err := content.ResolveResource(desired)
	if err != nil {
		return "", false, err
	}
	if exists && blobStoreResourcesEqual(existing, prepared) {
		if dryRun {
			return provision.StatusUnchanged, false, nil
		}
		// The row already matches, but the ownership effect (declarative
		// adopt/refresh, or a forced transfer) must still be applied inside the
		// keyed ownership lock. Routing it through the owned save — where the
		// identical-definition update is a row no-op — closes the race where the
		// generic post-commit writer would recreate an ownership record after a
		// concurrent forced transfer released it.
		if err := controller.commands.SaveBlobStore(ctx, controlplane.SaveBlobStoreCommand{
			BlobStore: prepared,
			Create:    false,
			Intent:    intent,
		}); err != nil {
			return "", false, err
		}
		return provision.StatusUnchanged, true, nil
	}
	if exists && blobStoreRuntimeConfigurationEqual(existing, prepared) {
		if dryRun {
			return provision.StatusUpdated, false, nil
		}
		if err := controller.commands.SaveBlobStore(ctx, controlplane.SaveBlobStoreCommand{
			BlobStore: prepared,
			Create:    false,
			Intent:    intent,
		}); err != nil {
			return "", false, err
		}
		return provision.StatusUpdated, true, nil
	}
	if exists {
		// Past the unchanged and attribute-only branches, an existing store's
		// definition changed. Driver, configuration reference and physical
		// destination are fixed for a store's lifetime, so report the
		// immutable-field conflict instead of repointing the backend or
		// recreating the resource; changing it means a new store and a drain.
		// This holds for the default store and named stores alike.
		return "", false, domain.ErrBlobStoreDefinitionImmutable
	}
	// Only a new definition needs a backend readiness check. Existing resources
	// were compared using their canonical physical identity without reopening a
	// backend for an unchanged, attribute-only, or rejected immutable update.
	prepared, opened, err := controller.server.blobStores.CheckReady(ctx, prepared, configuration)
	if err != nil {
		return "", false, err
	}
	if dryRun {
		controller.mu.Lock()
		otherName, duplicate := controller.preflightIdentity[prepared.PhysicalIdentity]
		if !duplicate {
			controller.preflightIdentity[prepared.PhysicalIdentity] = prepared.Name
		}
		controller.mu.Unlock()
		if duplicate && otherName != prepared.Name {
			content.CloseStore(opened)
			return "", false, domain.ErrBlobStoreIdentityConflict
		}
	}
	configuredStores, err := controller.server.blobStoreReads().BlobStores(ctx)
	if err != nil {
		content.CloseStore(opened)
		return "", false, err
	}
	for _, configured := range configuredStores {
		if configured.Name != desired.Name &&
			configured.PhysicalIdentity == prepared.PhysicalIdentity {
			content.CloseStore(opened)
			return "", false, domain.ErrBlobStoreIdentityConflict
		}
	}
	if dryRun {
		content.CloseStore(opened)
		return provision.StatusCreated, false, nil
	}
	// The backend is ready; commit the row and its ownership record in one
	// transaction, then remember the open backend. A crash after the commit
	// leaves a persisted store whose backend is reopened on the next reconcile,
	// so the remember is a self-healing post-commit effect.
	if err := controller.commands.SaveBlobStore(ctx, controlplane.SaveBlobStoreCommand{
		BlobStore: prepared,
		Create:    true,
		Intent:    intent,
	}); err != nil {
		content.CloseStore(opened)
		return "", false, err
	}
	controller.server.blobStores.Remember(prepared, opened)
	return provision.StatusCreated, true, nil
}

func (controller *provisionBlobStoreController) DeleteBlobStore(
	ctx context.Context,
	name string,
	intent controlplane.Intent,
	dryRun bool,
) error {
	if dryRun {
		_, err := controller.server.blobStoreReads().BlobStore(ctx, name)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if name == "default" {
			return domain.ErrDefaultBlobStoreImmutable
		}
		return nil
	}
	return controller.server.content.WithBlobStoreLease(ctx, name, func(leaseCtx context.Context) error {
		if err := controller.commands.DeleteBlobStore(leaseCtx, name, intent); err != nil {
			return err
		}
		controller.server.blobStores.Forget(name)
		return nil
	})
}

func (s *Server) provisionEngine() provision.Engine {
	blobStores := &provisionBlobStoreController{
		server:            s,
		commands:          controlplane.NewBlobStoreService(s.metadata),
		preflightIdentity: make(map[string]string),
	}
	// Build the command ports over s.metadata (as Store is) rather than the
	// New-time fields, so the engine and its mutations share one backend.
	return provision.Engine{
		Store:               s.metadata,
		BlobStores:          blobStores,
		Accounts:            controlplane.NewAccountService(s.metadata),
		OIDC:                controlplane.NewOIDCService(s.metadata),
		Roles:               controlplane.NewRoleService(s.metadata),
		Repositories:        controlplane.NewRepositoryService(s.metadata),
		CleanupPolicies:     controlplane.NewCleanupPolicyService(s.metadata),
		TrustPolicies:       controlplane.NewTrustPolicyService(s.metadata),
		Classifications:     controlplane.NewClassificationService(s.metadata),
		DownloadGates:       controlplane.NewDownloadGateService(s.metadata),
		Webhooks:            controlplane.NewWebhookService(s.metadata),
		Records:             s.metadata,
		RepositoryReads:     s.metadata,
		RoleReads:           s.metadata,
		AccountReads:        s.metadata,
		OIDCReads:           s.metadata,
		CleanupPolicyReads:  s.metadata,
		ClassificationReads: s.metadata,
		TrustPolicyReads:    s.metadata,
		DownloadGateReads:   s.metadata,
		WebhookReads:        s.metadata,
		BlobStoreReads:      s.metadata,
		Defaults:            provision.BuiltInResources(),
	}
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listManagedResources(w, r)
	case http.MethodPost:
		s.applyProvisionDocument(w, r)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listManagedResources(w http.ResponseWriter, r *http.Request) {
	records, err := s.provisionRecordReads().ProvisionRecords(r.Context())
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	resources := make([]managedResourceRecord, len(records))
	for index, record := range records {
		resources[index] = managedResourceRecord{
			Kind:      record.Kind,
			Name:      record.Name,
			UpdatedAt: record.UpdatedAt,
		}
	}
	httpx.WriteCollection(w, r, "managed-resources", resources, func(resource managedResourceRecord) string {
		return resource.Kind + "\x00" + resource.Name
	})
}

func (s *Server) applyProvisionDocument(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" && mediaType != "application/yaml" {
		httpx.WriteProblem(
			w,
			http.StatusUnsupportedMediaType,
			"unsupported_media_type",
			"provisioning requires application/json or application/yaml",
		)
		return
	}
	var document provision.Document
	if mediaType == "application/json" {
		document, err = provision.ParseCanonicalJSON(r.Body)
	} else {
		document, err = provision.ParseCanonical(r.Body)
	}
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_provisioning_document", err.Error())
		return
	}
	document, err = provision.ResolveSecrets(document, provision.EnvironmentResolver{})
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_secret_reference", err.Error())
		return
	}
	dryRun, err := queryBoolean(r, "dryRun")
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	prune, err := queryBoolean(r, "prune")
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	report, err := s.provisionEngine().Apply(r.Context(), document, provision.Options{
		DryRun: dryRun,
		Prune:  prune,
	})
	if err != nil {
		if detail, safe := provision.PublicError(err); safe {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_provisioning_document", detail)
		} else {
			httpx.WriteServerProblem(w, http.StatusServiceUnavailable, "provisioning_unavailable", "provisioning temporarily unavailable", err)
		}
		return
	}
	// OCI listener ports are bound at startup; any newly provisioned port opens
	// on the next restart, while routing changes take effect now.
	report = report.Redacted(func(result provision.Result, cause error) {
		httpx.RequestLogger(s.log, r).Error("provisioning resource failed",
			"kind", result.Kind, "name", result.Name, "error", cause)
	})
	httpx.WriteJSON(w, http.StatusOK, report)
}

func (s *Server) managedResourceNames(
	ctx context.Context,
	kind string,
) (map[string]struct{}, error) {
	records, err := s.provisionRecordReads().ProvisionRecords(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	for _, record := range records {
		if record.Kind == kind {
			names[record.Name] = struct{}{}
		}
	}
	return names, nil
}

func (s *Server) resourceIsManaged(
	ctx context.Context,
	kind string,
	name string,
) (bool, error) {
	_, err := s.provisionRecordReads().ProvisionRecord(ctx, kind, name)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// prepareManagedResourceMutation rejects configuration drift unless the caller
// explicitly requests ownership transfer with force=true. The returned flag
// tells the successful mutation path to remove the provisioning record.
func (s *Server) prepareManagedResourceMutation(
	w http.ResponseWriter,
	r *http.Request,
	kind string,
	name string,
) (releaseOwnership bool, allowed bool) {
	managed, err := s.resourceIsManaged(r.Context(), kind, name)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return false, false
	}
	if !managed {
		return false, true
	}
	force, err := queryBoolean(r, "force")
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
		return false, false
	}
	if !force {
		httpx.WriteProblem(
			w,
			http.StatusConflict,
			"managed_resource",
			fmt.Sprintf("%s %q is managed by declarative provisioning", kind, name),
		)
		return false, false
	}
	return true, true
}

// rejectManagedMutation is the read-only precedence guard for a control-plane
// kind whose imperative PUT validates the request body before mutating: it
// writes 409 and returns false when the resource is provisioning-managed and
// force is not set, so a managed resource is refused regardless of body
// validity. The store repeats the guard atomically inside the mutation
// transaction; this early copy only fixes error precedence.
func (s *Server) rejectManagedMutation(w http.ResponseWriter, r *http.Request, kind, name string) (allowed bool) {
	_, allowed = s.prepareManagedResourceMutation(w, r, kind, name)
	return allowed
}

func managedName(names map[string]struct{}, name string) bool {
	_, managed := names[name]
	return managed
}

func queryBoolean(r *http.Request, name string) (bool, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

// ApplyProvisionSource waits for and renews the cluster provisioning lease, so
// every replica observes a completed reconciliation before it starts serving.
func (s *Server) ApplyProvisionSource(
	ctx context.Context,
	source string,
) (report provision.Report, applied bool, err error) {
	document, err := loadProvisionSource(source)
	if err != nil {
		return report, false, err
	}
	document, err = provision.ResolveSecrets(document, provision.EnvironmentResolver{})
	if err != nil {
		return report, false, err
	}
	startupContext, cancelStartup := context.WithTimeout(ctx, provisionStartupDeadline)
	defer cancelStartup()
	if err := s.waitForProvisionLease(startupContext); err != nil {
		return report, false, startup.Retry(err)
	}

	applyContext, cancelApply := context.WithCancel(startupContext)
	stopRenewal := make(chan struct{})
	renewalDone := make(chan error, 1)
	go s.renewProvisionLease(applyContext, cancelApply, stopRenewal, renewalDone)
	engine := s.provisionEngine()
	preview, previewErr := engine.Apply(
		applyContext,
		document,
		provision.Options{DryRun: true},
	)
	applied = true
	switch {
	case previewErr != nil:
		err = classifyProvisionError(previewErr)
	case preview.Failed():
		report = preview
		err = fmt.Errorf("startup provisioning preflight failed: %w", classifyProvisionReport(preview))
	case !provisionReportHasChanges(preview):
		var needsOwnership bool
		needsOwnership, err = s.provisionReportNeedsOwnership(applyContext, preview)
		if err == nil && needsOwnership {
			report, err = engine.Apply(applyContext, document, provision.Options{})
		} else if err == nil {
			preview.DryRun = false
			report = preview
			applied = false
		}
	default:
		report, err = engine.Apply(applyContext, document, provision.Options{})
	}
	if err == nil && report.Failed() {
		err = fmt.Errorf("startup provisioning failed: %w", classifyProvisionReport(report))
	}
	close(stopRenewal)
	renewalErr := <-renewalDone
	cancelApply()
	releaseErr := s.releaseProvisionLease()
	if err != nil {
		return report, applied, classifyProvisionError(err)
	}
	if renewalErr != nil {
		return report, applied, startup.Retry(renewalErr)
	}
	if releaseErr != nil {
		return report, applied, startup.Retry(releaseErr)
	}
	return report, applied, nil
}

// Even an unchanged resource needs a write pass when it was created through
// the imperative API; otherwise startup silently skips adopting ownership.
func (s *Server) provisionReportNeedsOwnership(ctx context.Context, report provision.Report) (bool, error) {
	for _, result := range report.Results {
		_, err := s.provisionRecordReads().ProvisionRecord(ctx, result.Kind, result.Name)
		if errors.Is(err, domain.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read provisioning ownership for %s %q: %w", result.Kind, result.Name, err)
		}
	}
	return false, nil
}

func provisionReportHasChanges(report provision.Report) bool {
	for _, result := range report.Results {
		if result.Status != provision.StatusUnchanged {
			return true
		}
	}
	return false
}

func (s *Server) waitForProvisionLease(ctx context.Context) error {
	for {
		now := time.Now().UTC()
		acquired, err := s.leases().AcquireLease(
			ctx,
			provisionLeaseName,
			s.schedulerID,
			now,
			now.Add(provisionLeaseDuration),
		)
		if err != nil {
			return fmt.Errorf("acquire provisioning lease: %w", err)
		}
		if acquired {
			s.metrics.holdLeaderUntil(provisioningMetricRole, now.Add(provisionLeaseDuration))
			return nil
		}
		timer := time.NewTimer(provisionLeaseRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for provisioning lease: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *Server) renewProvisionLease(
	ctx context.Context,
	cancelApply context.CancelFunc,
	stop <-chan struct{},
	done chan<- error,
) {
	s.renewProvisionLeaseWithTiming(
		ctx,
		cancelApply,
		stop,
		done,
		provisionLeaseRenewal,
		provisionLeaseDuration,
	)
}

func (s *Server) renewProvisionLeaseWithTiming(
	ctx context.Context,
	cancelApply context.CancelFunc,
	stop <-chan struct{},
	done chan<- error,
	renewalInterval time.Duration,
	leaseDuration time.Duration,
) {
	ticker := time.NewTicker(renewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			done <- nil
			return
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case <-ticker.C:
			now := time.Now().UTC()
			acquired, err := s.leases().AcquireLease(
				ctx,
				provisionLeaseName,
				s.schedulerID,
				now,
				now.Add(leaseDuration),
			)
			if err != nil {
				cancelApply()
				done <- fmt.Errorf("renew provisioning lease: %w", err)
				return
			}
			if !acquired {
				s.metrics.releaseLeader(provisioningMetricRole)
				cancelApply()
				done <- errors.New("provisioning lease was lost during reconciliation")
				return
			}
			s.metrics.holdLeaderUntil(
				provisioningMetricRole,
				now.Add(leaseDuration),
			)
		}
	}
}

func (s *Server) releaseProvisionLease() error {
	releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releasedAt := time.Now().UTC()
	acquired, err := s.leases().AcquireLease(
		releaseContext,
		provisionLeaseName,
		s.schedulerID,
		releasedAt,
		releasedAt,
	)
	if err != nil {
		return fmt.Errorf("release provisioning lease: %w", err)
	}
	if !acquired {
		return errors.New("release provisioning lease: lease ownership was lost")
	}
	s.metrics.releaseLeader(provisioningMetricRole)
	return nil
}

func loadProvisionSource(source string) (provision.Document, error) {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" {
		return provision.Document{}, errors.New("SUXEN_PROVISION must be a local file:// URL")
	}
	path := parsed.Path
	if !filepath.IsAbs(path) {
		return provision.Document{}, errors.New("SUXEN_PROVISION file path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return provision.Document{}, fmt.Errorf("stat provisioning source: %w", err)
	}
	if !info.IsDir() {
		return parseProvisionFile(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return provision.Document{}, fmt.Errorf("read provisioning directory: %w", err)
	}
	fileNames := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension == ".yaml" || extension == ".yml" ||
			extension == ".json" || extension == ".jsonl" {
			fileNames = append(fileNames, entry.Name())
		}
	}
	sort.Strings(fileNames)
	if len(fileNames) == 0 {
		return provision.Document{}, errors.New("provisioning directory has no YAML, JSON, or JSONL files")
	}
	combined := provision.Document{APIVersion: provision.APIVersion}
	seen := make(map[string]string)
	var aggregateSize int64
	for _, fileName := range fileNames {
		filePath := filepath.Join(path, fileName)
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			return provision.Document{}, fmt.Errorf("stat provisioning file %s: %w", filePath, err)
		}
		aggregateSize += fileInfo.Size()
		if aggregateSize > provision.MaxDocumentSize {
			return provision.Document{}, fmt.Errorf(
				"provisioning directory exceeds %d bytes",
				provision.MaxDocumentSize,
			)
		}
		document, err := parseProvisionFile(filePath)
		if err != nil {
			return provision.Document{}, err
		}
		for _, resource := range document.Resources {
			key := resource.Kind + "\x00" + resource.Name
			if previous, duplicate := seen[key]; duplicate {
				return provision.Document{}, fmt.Errorf(
					"duplicate %s %q in %s and %s",
					resource.Kind,
					resource.Name,
					previous,
					fileName,
				)
			}
			seen[key] = fileName
			combined.Resources = append(combined.Resources, resource)
		}
	}
	return combined, nil
}

func parseProvisionFile(path string) (provision.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return provision.Document{}, fmt.Errorf("open provisioning file %s: %w", path, err)
	}
	defer file.Close()
	document, err := provision.Parse(file)
	if err != nil {
		return provision.Document{}, fmt.Errorf("parse provisioning file %s: %w", path, err)
	}
	return document, nil
}
