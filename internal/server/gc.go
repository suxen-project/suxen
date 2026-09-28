package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// blobReferenceGC is the blob-reference garbage-collection capability the gc
// worker, verification, and usage reporting need: the set of digests a blob
// store still references, and deletion of a store's unreferenced blob assets.
// Both are keyed by blob store, not by repository.
type blobReferenceGC interface {
	ReferencedDigests(context.Context, string) (map[string]struct{}, error)
	DeleteUnreferencedBlobAssets(context.Context, string, string) (int64, error)
}

// blobReferenceGC narrows the metadata store to the blob-reference GC
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) blobReferenceGC() blobReferenceGC {
	return s.metadata
}

const defaultGCGracePeriod = 24 * time.Hour

var errBlobStoreGCLeaseSetChanged = errors.New("blob-store GC lease set changed")

type garbageCollectionResult struct {
	DryRun      bool     `json:"dryRun"`
	BlobStore   string   `json:"blobStore,omitempty"`
	Scanned     int      `json:"scanned"`
	Referenced  int      `json:"referenced"`
	Deleted     int      `json:"deleted"`
	Reclaimed   int64    `json:"reclaimedBytes"`
	WouldDelete []string `json:"wouldDelete,omitempty"`
	GracePeriod string   `json:"gracePeriod"`
	// StaleUploads counts upload sessions idle beyond their store's staleAfter window.
	StaleUploads int `json:"staleUploads"`
	// StaleUploadsDeleted counts the stale sessions removed with their staged object.
	StaleUploadsDeleted      int `json:"staleUploadsDeleted"`
	StaleStagingFiles        int `json:"staleStagingFiles"`
	StaleStagingFilesDeleted int `json:"staleStagingFilesDeleted"`
}

func (s *Server) handleGarbageCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}

	dryRun, err := strconv.ParseBool(defaultString(r.URL.Query().Get("dryRun"), "true"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_dry_run", err.Error())
		return
	}
	gracePeriod, err := parseGracePeriod(r.URL.Query().Get("grace"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_grace_period", err.Error())
		return
	}

	result, err := s.runGarbageCollection(r.Context(), dryRun, gracePeriod, "")
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"gc_failed",
			"garbage collection failed",
			err,
		)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) runGarbageCollection(
	ctx context.Context,
	dryRun bool,
	gracePeriod time.Duration,
	storeName string,
) (garbageCollectionResult, error) {
	startedAt := time.Now().UTC()
	task, err := s.tasks().CreateTask(ctx, domain.Task{
		Type:      "garbage-collection",
		Status:    "running",
		DryRun:    dryRun,
		StartedAt: &startedAt,
	})
	if err != nil {
		return garbageCollectionResult{}, err
	}
	result, runErr := s.collectGarbage(ctx, dryRun, gracePeriod, startedAt, storeName)
	s.metrics.addGarbageCollected(result.Deleted)
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
			return result, fmt.Errorf("%w; update task: %w", runErr, updateErr)
		}
		return result, updateErr
	}
	return result, runErr
}

func (s *Server) collectGarbage(
	ctx context.Context,
	dryRun bool,
	gracePeriod time.Duration,
	now time.Time,
	storeName string,
) (garbageCollectionResult, error) {
	// An empty storeName sweeps every store; a set name scopes collection to one
	// physical store, whose digests are isolated from the others.
	var resources []domain.BlobStore
	if storeName == "" {
		all, err := s.blobStoreReads().BlobStores(ctx)
		if err != nil {
			return garbageCollectionResult{}, fmt.Errorf("list blob stores: %w", err)
		}
		resources = all
	} else {
		store, err := s.blobStoreReads().BlobStore(ctx, storeName)
		if err != nil {
			return garbageCollectionResult{}, err
		}
		resources = []domain.BlobStore{store}
	}

	result := garbageCollectionResult{
		DryRun:      dryRun,
		BlobStore:   storeName,
		GracePeriod: gracePeriod.String(),
	}
	// Abandoned upload sessions count against principal quotas until removed, so they
	// are swept before blob collection, which can fail independently. Sweeping them
	// is a global operation, so it is skipped when collection is scoped to one store.
	if storeName == "" {
		stale, deleted, err := s.content.ReapStaleStaging(dryRun, now)
		result.StaleStagingFiles = stale
		result.StaleStagingFilesDeleted = deleted
		if err != nil {
			return result, fmt.Errorf("reap staged uploads: %w", err)
		}
		staleUploads, err := s.oci.ReapStaleUploadSessions(ctx, dryRun, now)
		result.StaleUploads = staleUploads.Stale
		result.StaleUploadsDeleted = staleUploads.Deleted
		if err != nil {
			return result, fmt.Errorf("reap stale upload sessions: %w", err)
		}
	}
	deleteBefore := now.Add(-gracePeriod)
	for _, initialResource := range resources {
		resource := initialResource
		for {
			leaseNames := []string{resource.Name}
			if resource.State == domain.BlobStoreStateDraining && resource.DrainTarget != "" {
				leaseNames = append(leaseNames, resource.DrainTarget)
			}
			err := s.content.WithBlobStoreLeases(ctx, leaseNames, func(leaseCtx context.Context) error {
				current, err := s.blobStoreReads().BlobStore(leaseCtx, resource.Name)
				if err != nil {
					return err
				}
				if current.State == domain.BlobStoreStateDraining &&
					current.DrainTarget != "" &&
					!containsString(leaseNames, current.DrainTarget) {
					resource = current
					return errBlobStoreGCLeaseSetChanged
				}
				resource = current
				referenced, err := s.blobReferenceGC().ReferencedDigests(leaseCtx, resource.Name)
				if err != nil {
					return fmt.Errorf(
						"list referenced digests for blob store %q: %w",
						resource.Name,
						err,
					)
				}
				configuredStore, err := s.blobStores.Store(leaseCtx, resource.Name)
				if err != nil {
					return err
				}
				result.Referenced += len(referenced)
				// Migration owns reclamation from a draining source: it copies every
				// blob to the target before deleting the source. Keeping GC read-only
				// in this state prevents a reference snapshot from racing a manifest
				// publication that is routed directly to the drain target.
				err = configuredStore.Walk(leaseCtx, func(candidate domain.BlobInfo) error {
					result.Scanned++
					if resource.State == domain.BlobStoreStateDraining {
						return nil
					}
					if _, retained := referenced[candidate.Digest]; retained {
						return nil
					}
					if candidate.ModifiedAt.After(deleteBefore) {
						return nil
					}
					if dryRun {
						candidateName := resource.Name + ":" + candidate.Digest
						if len(resources) == 1 && resource.Name == "default" {
							candidateName = candidate.Digest
						}
						result.WouldDelete = append(
							result.WouldDelete,
							candidateName,
						)
						return nil
					}
					// Remove unreferenced OCI metadata first. If this database write
					// fails, the physical blob remains readable; if the physical
					// delete then fails, the orphan is visible to a later GC walk.
					// Reversing the order strands metadata pointing at a missing
					// blob when the database write fails after physical deletion.
					if _, err := s.blobReferenceGC().DeleteUnreferencedBlobAssets(
						leaseCtx,
						resource.Name,
						candidate.Digest,
					); err != nil {
						return fmt.Errorf("delete blob metadata %s: %w", candidate.Digest, err)
					}
					if err := configuredStore.Delete(leaseCtx, candidate.Digest); err != nil {
						return fmt.Errorf("delete blob %s: %w", candidate.Digest, err)
					}
					result.Deleted++
					result.Reclaimed += candidate.Size
					return nil
				})
				if err != nil {
					return fmt.Errorf("walk blobs in %q: %w", resource.Name, err)
				}
				return nil
			})
			if errors.Is(err, errBlobStoreGCLeaseSetChanged) {
				continue
			}
			if err != nil {
				return result, err
			}
			break
		}
	}
	return result, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func parseGracePeriod(value string) (time.Duration, error) {
	if value == "" {
		return defaultGCGracePeriod, nil
	}
	gracePeriod, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if gracePeriod < 0 {
		return 0, fmt.Errorf("grace period cannot be negative")
	}
	return gracePeriod, nil
}

func defaultString(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}
