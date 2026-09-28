package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
)

// uploadSessions narrows the metadata store to the in-flight upload-session
// ledger the drain path consults: a draining source is not reclaimed while it
// still has staged OCI upload sessions, since those are not ordinary blobs and
// must finish or be reaped first. It reads s.metadata on each call so a
// reconfigured backend is honoured.
type uploadSessions interface {
	CountUploadSessions(ctx context.Context, blobStore string) (int64, error)
}

func (s *Server) uploadSessions() uploadSessions {
	return s.metadata
}

// blobStoreMigrationResult is one migrate run's progress across every draining
// store.
type blobStoreMigrationResult struct {
	Stores  []storeMigrationProgress `json:"stores"`
	Copied  int                      `json:"copied"`
	Deleted int                      `json:"deleted"`
}

type storeMigrationProgress struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Copied int    `json:"copied"`
	// Deleted counts blobs removed from the source; Remaining counts blobs still
	// on the source (not yet confirmed on the target). Drained is true once the
	// source holds nothing and its state was advanced.
	Deleted   int `json:"deleted"`
	Remaining int `json:"remaining"`
	// PendingUploads keeps a draining source bound while resumable sessions
	// still depend on its staged objects.
	PendingUploads int64  `json:"pendingUploads,omitempty"`
	Drained        bool   `json:"drained"`
	Error          string `json:"error,omitempty"`
}

// runBlobStoreMigration advances every draining blob store one step: it copies
// the source's blobs to the target, rebinds the source's repositories, deletes
// what is safely on the target, and marks the source drained once it is empty.
//
// Each step is idempotent, so an interrupted run resumes on the next tick. The
// operation lease and active-upload guard mean no source publication lands
// between the copy and the rebind, so a single copy pass is sufficient.
func (s *Server) runBlobStoreMigration(ctx context.Context) error {
	stores, err := s.blobStoreReads().BlobStores(ctx)
	if err != nil {
		return fmt.Errorf("list blob stores: %w", err)
	}
	var draining []domain.BlobStore
	for _, resource := range stores {
		if resource.State == domain.BlobStoreStateDraining {
			draining = append(draining, resource)
		}
	}
	if len(draining) == 0 {
		return nil
	}

	startedAt := time.Now().UTC()
	task, err := s.tasks().CreateTask(ctx, domain.Task{
		Type:      "blob-store-migration",
		Status:    "running",
		StartedAt: &startedAt,
	})
	if err != nil {
		return err
	}

	result, runErr := s.migrateDrainingStores(ctx, draining)
	s.metrics.addBlobMigration(result.Copied, result.Deleted)
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
			return fmt.Errorf("%w; update task: %w", runErr, updateErr)
		}
		return updateErr
	}
	return runErr
}

func (s *Server) migrateDrainingStores(
	ctx context.Context,
	draining []domain.BlobStore,
) (blobStoreMigrationResult, error) {
	var result blobStoreMigrationResult
	var firstErr error
	for _, source := range draining {
		progress, err := s.migrateStore(ctx, source)
		if err != nil {
			// One store's failure must not stop the others; record it and keep
			// going, surfacing the first error to fail the task.
			progress.Error = err.Error()
			s.log.Error("migrate blob store", "blobStore", source.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		result.Stores = append(result.Stores, progress)
		result.Copied += progress.Copied
		result.Deleted += progress.Deleted
	}
	return result, firstErr
}

func (s *Server) migrateStore(
	ctx context.Context,
	source domain.BlobStore,
) (storeMigrationProgress, error) {
	var progress storeMigrationProgress
	// The list entry is only a hint. Drain cancellation and a new drain can
	// change the target before we acquire the operation leases.
	current, err := s.blobStoreReads().BlobStore(ctx, source.Name)
	if err != nil {
		return storeMigrationProgress{Source: source.Name}, err
	}
	if current.State != domain.BlobStoreStateDraining {
		return storeMigrationProgress{Source: source.Name}, nil
	}
	if current.DrainTarget == "" {
		return storeMigrationProgress{Source: source.Name}, errors.New("draining store has no drain target")
	}
	for {
		var changed bool
		err = s.content.WithBlobStoreLeases(
			ctx,
			[]string{source.Name, current.DrainTarget},
			func(leaseCtx context.Context) error {
				lockedSource, err := s.blobStoreReads().BlobStore(leaseCtx, source.Name)
				if err != nil {
					return err
				}
				if lockedSource.State != domain.BlobStoreStateDraining {
					progress = storeMigrationProgress{Source: source.Name}
					return nil
				}
				if lockedSource.DrainTarget != current.DrainTarget {
					current = lockedSource
					changed = true
					return nil
				}
				progress, err = s.migrateStoreLocked(leaseCtx, lockedSource)
				return err
			},
		)
		if err != nil || !changed {
			return progress, err
		}
		if current.DrainTarget == "" {
			return storeMigrationProgress{Source: source.Name}, errors.New("draining store has no drain target")
		}
	}
}

func (s *Server) migrateStoreLocked(
	ctx context.Context,
	source domain.BlobStore,
) (storeMigrationProgress, error) {
	progress := storeMigrationProgress{Source: source.Name, Target: source.DrainTarget}
	if source.DrainTarget == "" {
		return progress, errors.New("draining store has no drain target")
	}
	target, err := s.blobStoreReads().BlobStore(ctx, source.DrainTarget)
	if err != nil {
		return progress, fmt.Errorf("resolve drain target %q: %w", source.DrainTarget, err)
	}
	if target.State != domain.BlobStoreStateActive {
		return progress, fmt.Errorf("drain target %q is not active", source.DrainTarget)
	}
	// Staged OCI uploads are not ordinary blobs. Keep the source binding and
	// physical store available until those sessions finish or the reaper expires
	// them; new sessions on a draining binding are staged in the target.
	activeUploads, err := s.uploadSessions().CountUploadSessions(ctx, source.Name)
	if err != nil {
		return progress, err
	}
	if activeUploads != 0 {
		progress.PendingUploads = activeUploads
		return progress, nil
	}

	sourceStore, err := s.blobStores.Store(ctx, source.Name)
	if err != nil {
		return progress, err
	}
	targetStore, err := s.blobStores.Store(ctx, source.DrainTarget)
	if err != nil {
		return progress, err
	}

	// Copy everything the source physically holds to the target before any
	// binding moves, so a read never resolves to a store missing the blob.
	copied, err := copyMissingBlobs(ctx, sourceStore, targetStore)
	progress.Copied += copied
	if err != nil {
		return progress, err
	}

	// Move the repositories. The operation lease blocks source publications for
	// the duration of this step and the active-upload guard above rules out staged
	// sessions, so the copy above already holds everything the source has; after
	// the rebind new writes target the drain target, never the source.
	if _, err := s.blobStoreAdmin().RebindRepositories(ctx, source.Name, source.DrainTarget); err != nil {
		return progress, fmt.Errorf("rebind repositories: %w", err)
	}

	// Delete from the source only what is confirmed present on the target.
	deleted, remaining, err := drainSourceBlobs(ctx, sourceStore, targetStore)
	progress.Deleted = deleted
	progress.Remaining = remaining
	if err != nil {
		return progress, err
	}
	if remaining == 0 {
		if err := s.blobStoreAdmin().SetBlobStoreState(
			ctx,
			source.Name,
			domain.BlobStoreStateDrained,
			source.DrainTarget,
		); err != nil {
			return progress, err
		}
		progress.Drained = true
		s.log.Info("blob store drained", "blobStore", source.Name, "target", source.DrainTarget)
	}
	return progress, nil
}

// copyMissingBlobs copies every blob present in from but absent from to. It is
// idempotent: a blob already on the target is skipped by a Head check.
func copyMissingBlobs(ctx context.Context, from blob.Store, to blob.Store) (int, error) {
	copied := 0
	err := from.Walk(ctx, func(info domain.BlobInfo) error {
		if existing, err := to.Head(ctx, info.Digest); err == nil {
			// Put deduplicates an existing digest key, so it cannot repair a
			// truncated target. Fail before rebind instead of trusting it.
			if existing.Size != info.Size {
				return fmt.Errorf("target blob %s has size %d, source has %d", info.Digest, existing.Size, info.Size)
			}
			return nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		reader, _, err := from.Get(ctx, info.Digest)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil
			}
			return err
		}
		_, putErr := to.Put(ctx, info.Digest, reader)
		_ = reader.Close()
		if putErr != nil {
			return fmt.Errorf("copy blob %s: %w", info.Digest, putErr)
		}
		copied++
		return nil
	})
	return copied, err
}

// drainSourceBlobs deletes from the source every blob confirmed present on the
// target and reports how many could not yet be confirmed (and were kept).
func drainSourceBlobs(ctx context.Context, source blob.Store, target blob.Store) (int, int, error) {
	deleted := 0
	remaining := 0
	err := source.Walk(ctx, func(info domain.BlobInfo) error {
		existing, err := target.Head(ctx, info.Digest)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				remaining++
				return nil
			}
			return err
		}
		if existing.Size != info.Size {
			// Keep the source copy when an existing target key is incomplete.
			return fmt.Errorf("target blob %s has size %d, source has %d", info.Digest, existing.Size, info.Size)
		}
		if err := source.Delete(ctx, info.Digest); err != nil {
			return fmt.Errorf("delete source blob %s: %w", info.Digest, err)
		}
		deleted++
		return nil
	})
	return deleted, remaining, err
}
