package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

// maxVerifyFindingsListed caps the per-run finding lists carried in the task
// result so a store with a large backlog cannot produce an unbounded payload;
// the *Total counters still report the full magnitude.
const maxVerifyFindingsListed = 200

// verifyFinding locates one blob a verify run flagged.
type verifyFinding struct {
	BlobStore string `json:"blobStore"`
	Digest    string `json:"digest"`
}

// blobStoreVerifyResult is the read-only report of one verify run. Verify never
// mutates: it cross-checks each store's physical contents against the digests
// the metadata references, and optionally re-hashes present-and-referenced
// blobs to catch bit-rot.
type blobStoreVerifyResult struct {
	Rehash    bool   `json:"rehash"`
	BlobStore string `json:"blobStore,omitempty"` // empty means every store
	Stores    int    `json:"stores"`
	// Scanned counts blobs physically present; Referenced counts distinct
	// digests the metadata expects; Rehashed counts blobs actually re-hashed.
	Scanned    int `json:"scanned"`
	Referenced int `json:"referenced"`
	Rehashed   int `json:"rehashed"`
	// Dangling: referenced by metadata but absent from the store (a read would
	// fail). Orphaned: present but unreferenced (gc reclaims these). Mismatched:
	// present content whose recomputed digest differs from its key.
	DanglingTotal   int             `json:"danglingTotal"`
	OrphanedTotal   int             `json:"orphanedTotal"`
	MismatchedTotal int             `json:"mismatchedTotal"`
	Dangling        []verifyFinding `json:"dangling,omitempty"`
	Mismatched      []verifyFinding `json:"mismatched,omitempty"`
}

func (s *Server) handleBlobStoreVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.MethodNotAllowed(w, http.MethodPost)
		return
	}

	rehash, err := strconv.ParseBool(defaultString(r.URL.Query().Get("rehash"), "false"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_rehash", err.Error())
		return
	}

	blobStore := r.URL.Query().Get("blobStore")
	if blobStore != "" {
		if _, err := s.blobStoreReads().BlobStore(r.Context(), blobStore); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				httpx.WriteProblem(w, http.StatusNotFound, "blob_store_not_found", "blob store not found")
				return
			}
			httpx.WriteServerProblem(
				w,
				http.StatusInternalServerError,
				"verify_failed",
				"blob store verification failed",
				err,
			)
			return
		}
	}

	result, err := s.runBlobStoreVerify(r.Context(), blobStore, rehash)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"verify_failed",
			"blob store verification failed",
			err,
		)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) runBlobStoreVerify(
	ctx context.Context,
	blobStoreName string,
	rehash bool,
) (blobStoreVerifyResult, error) {
	startedAt := time.Now().UTC()
	task, err := s.tasks().CreateTask(ctx, domain.Task{
		Type:      "blob-store-verify",
		Status:    "running",
		StartedAt: &startedAt,
	})
	if err != nil {
		return blobStoreVerifyResult{}, err
	}
	result, runErr := s.verifyBlobStores(ctx, blobStoreName, rehash)
	// Only a full-scope run reflects the whole system, so only it updates the
	// gauge; a single-store run would otherwise zero the other stores' counts.
	if runErr == nil && blobStoreName == "" {
		s.metrics.setVerifyFindings(
			result.DanglingTotal,
			result.OrphanedTotal,
			result.MismatchedTotal,
		)
	}
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

func (s *Server) verifyBlobStores(
	ctx context.Context,
	blobStoreName string,
	rehash bool,
) (blobStoreVerifyResult, error) {
	var resources []domain.BlobStore
	if blobStoreName != "" {
		resource, err := s.blobStoreReads().BlobStore(ctx, blobStoreName)
		if err != nil {
			return blobStoreVerifyResult{}, err
		}
		resources = []domain.BlobStore{resource}
	} else {
		all, err := s.blobStoreReads().BlobStores(ctx)
		if err != nil {
			return blobStoreVerifyResult{}, fmt.Errorf("list blob stores: %w", err)
		}
		resources = all
	}

	result := blobStoreVerifyResult{
		Rehash:    rehash,
		BlobStore: blobStoreName,
		Stores:    len(resources),
	}
	for _, resource := range resources {
		if err := s.verifyBlobStore(ctx, resource.Name, rehash, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Server) verifyBlobStore(
	ctx context.Context,
	name string,
	rehash bool,
	result *blobStoreVerifyResult,
) error {
	referenced, err := s.metadata.PublishedReferencedDigests(ctx, name)
	if err != nil {
		return fmt.Errorf("list referenced digests for blob store %q: %w", name, err)
	}
	store, err := s.blobStores.Store(ctx, name)
	if err != nil {
		return err
	}
	result.Referenced += len(referenced)
	// Remove each observed digest from the reference set. This keeps memory
	// proportional to metadata references rather than all physical blobs.
	forEachErr := store.Walk(ctx, func(info domain.BlobInfo) error {
		result.Scanned++
		if _, isReferenced := referenced[info.Digest]; !isReferenced {
			result.OrphanedTotal++
			return nil
		}
		if !rehash {
			delete(referenced, info.Digest)
			return nil
		}
		matches, err := rehashBlob(ctx, store, info.Digest)
		if errors.Is(err, domain.ErrNotFound) {
			// GC or migration may have removed it since Walk observed it.
			// Confirm any remaining reference under the operation lease below.
			return nil
		}
		if err != nil {
			return fmt.Errorf("re-hash blob %s in %q: %w", info.Digest, name, err)
		}
		delete(referenced, info.Digest)
		result.Rehashed++
		if !matches {
			result.MismatchedTotal++
			if len(result.Mismatched) < maxVerifyFindingsListed {
				result.Mismatched = append(
					result.Mismatched,
					verifyFinding{BlobStore: name, Digest: info.Digest},
				)
			}
			s.log.Warn("blob content does not match its digest", "blobStore", name, "digest", info.Digest)
		}
		return nil
	})
	if forEachErr != nil {
		return fmt.Errorf("walk blobs in %q: %w", name, forEachErr)
	}

	return s.confirmDanglingBlobs(ctx, name, store, referenced, result)
}

// The scan deliberately runs without excluding publication, GC, or migration.
// Recheck suspected missing blobs against current metadata and physical storage
// while those operations are excluded, so a stale reference or listing cannot
// turn a normal concurrent operation into a corruption finding.
func (s *Server) confirmDanglingBlobs(
	ctx context.Context,
	name string,
	store blob.Store,
	candidates map[string]struct{},
	result *blobStoreVerifyResult,
) error {
	if len(candidates) == 0 {
		return nil
	}
	return s.content.WithBlobStoreLease(ctx, name, func(leaseCtx context.Context) error {
		current, err := s.metadata.PublishedReferencedDigests(leaseCtx, name)
		if err != nil {
			return fmt.Errorf("confirm referenced digests for blob store %q: %w", name, err)
		}
		for digest := range candidates {
			if _, exists := current[digest]; !exists {
				continue
			}
			if _, err := store.Head(leaseCtx, digest); err == nil {
				continue
			} else if !errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("confirm missing blob %s in %q: %w", digest, name, err)
			}
			result.DanglingTotal++
			if len(result.Dangling) < maxVerifyFindingsListed {
				result.Dangling = append(
					result.Dangling,
					verifyFinding{BlobStore: name, Digest: digest},
				)
			}
			s.log.Warn("referenced blob missing from store", "blobStore", name, "digest", digest)
		}
		return nil
	})
}

// rehashBlob streams a blob and reports whether its content still hashes to its
// key. A non-sha256 key is reported as matching: there is nothing to recompute
// it against, and verify must not manufacture a false mismatch.
func rehashBlob(ctx context.Context, store blob.Store, digest string) (bool, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return true, nil
	}
	reader, _, err := store.Get(ctx, digest)
	if err != nil {
		return false, err
	}
	defer reader.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == strings.TrimPrefix(digest, prefix), nil
}
