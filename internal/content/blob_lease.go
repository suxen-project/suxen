package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

const (
	blobStoreLeaseDuration = 30 * time.Second
	blobStoreLeaseRenewal  = 10 * time.Second
)

var errBlobStoreBindingChanged = errors.New("repository blob-store binding changed")

// ErrBlobStoreLeaseSetChanged asks a validated publication to recompute the
// stores it must lock. It is used when migration changed a dependency's home
// store between the initial dependency scan and lease acquisition.
var ErrBlobStoreLeaseSetChanged = errors.New("blob-store publication lease set changed")

// WithBlobStoreLease serializes publication, reclamation, and migration for a
// physical store through the shared metadata database. The callback receives a
// context cancelled if lease renewal fails.
func (rt *Runtime) WithBlobStoreLease(
	ctx context.Context,
	storeName string,
	operation func(context.Context) error,
) error {
	sum := sha256.Sum256([]byte(storeName))
	name := "blob-store-operation:" + hex.EncodeToString(sum[:])
	holder := rt.leaderID() + ":" + httpx.RandomSecret(12)
	if rt.leaderID() == "" {
		holder = "process:" + httpx.RandomSecret(12)
	}
	for {
		now := time.Now().UTC()
		acquired, err := rt.blobPlacement().AcquireLease(ctx, name, holder, now, now.Add(blobStoreLeaseDuration))
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer releaseCancel()
		_ = rt.blobPlacement().ReleaseLease(releaseCtx, name, holder)
	}()
	renewalError := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(blobStoreLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				now := time.Now().UTC()
				acquired, err := rt.blobPlacement().AcquireLease(
					leaseCtx, name, holder, now, now.Add(blobStoreLeaseDuration),
				)
				if err != nil || !acquired {
					if err == nil {
						err = fmt.Errorf("blob-store operation lease was lost")
					}
					renewalError <- err
					cancel()
					return
				}
			}
		}
	}()
	err := operation(leaseCtx)
	close(done)
	select {
	case renewalErr := <-renewalError:
		if err == nil {
			err = renewalErr
		}
	default:
	}
	return err
}

// WithBlobStoreLeases serializes an operation across several physical stores.
// Store names are de-duplicated and acquired in lexical order so overlapping
// multi-store operations cannot deadlock by requesting the same leases in a
// different order.
func (rt *Runtime) WithBlobStoreLeases(
	ctx context.Context,
	storeNames []string,
	operation func(context.Context) error,
) error {
	names := append([]string(nil), storeNames...)
	slices.Sort(names)
	names = slices.Compact(names)

	var acquire func(context.Context, int) error
	acquire = func(leaseCtx context.Context, index int) error {
		if index == len(names) {
			return operation(leaseCtx)
		}
		return rt.WithBlobStoreLease(leaseCtx, names[index], func(nextCtx context.Context) error {
			return acquire(nextCtx, index+1)
		})
	}
	return acquire(ctx, 0)
}

// CommitStagedAssets persists blobs and publishes all metadata while excluding
// GC and store migration. A draining binding writes directly to its target and
// records that physical store on every published asset.
func (rt *Runtime) CommitStagedAssets(
	ctx context.Context,
	repositoryName string,
	uploads []StagedUpload,
	assets []domain.Asset,
) ([]domain.BlobInfo, []domain.Asset, error) {
	return rt.CommitStagedAssetsValidated(ctx, repositoryName, uploads, assets, nil, nil)
}

// CommitStagedAssetsValidated publishes staged content while holding the write
// store lease plus every store returned by dependencyStores. validate runs only
// after those leases are held and immediately before the blob and metadata
// commit. This lets formats make dependency validation atomic with publication
// across store migration and garbage collection.
func (rt *Runtime) CommitStagedAssetsValidated(
	ctx context.Context,
	repositoryName string,
	uploads []StagedUpload,
	assets []domain.Asset,
	dependencyStores func(context.Context) ([]string, error),
	validate func(context.Context, map[string]struct{}) error,
) ([]domain.BlobInfo, []domain.Asset, error) {
	if len(uploads) == 0 || len(assets) == 0 {
		return nil, nil, fmt.Errorf("publication requires staged content and asset metadata")
	}
	stagedDigests := make(map[string]struct{}, len(uploads))
	for _, upload := range uploads {
		stagedDigests[upload.Digest] = struct{}{}
	}
	for _, asset := range assets {
		if _, found := stagedDigests[asset.Digest]; !found {
			return nil, nil, fmt.Errorf("asset %q has no staged content", asset.Path)
		}
	}
	repository, err := rt.repositoryResolver().Repository(ctx, repositoryName)
	if err != nil {
		return nil, nil, err
	}
	storeName, err := rt.blobPlacement().WriteBlobStore(ctx, repository.BlobStore)
	if err != nil {
		return nil, nil, err
	}
	var infos []domain.BlobInfo
	var stored []domain.Asset
	leaseNames := []string{storeName}
	if dependencyStores != nil {
		extra, dependencyErr := dependencyStores(ctx)
		if dependencyErr != nil {
			return nil, nil, dependencyErr
		}
		leaseNames = append(leaseNames, extra...)
	}
	err = rt.WithBlobStoreLeases(ctx, leaseNames, func(leaseCtx context.Context) error {
		// Resolve again under the lease; migration may have rebound the
		// repository or changed the drain target while this request staged its body.
		current, err := rt.repositoryResolver().Repository(leaseCtx, repositoryName)
		if err != nil {
			return err
		}
		currentStoreName, err := rt.blobPlacement().WriteBlobStore(leaseCtx, current.BlobStore)
		if err != nil {
			return err
		}
		if currentStoreName != storeName {
			return errBlobStoreBindingChanged
		}
		if validate != nil {
			locked := make(map[string]struct{}, len(leaseNames))
			for _, name := range leaseNames {
				locked[name] = struct{}{}
			}
			if err := validate(leaseCtx, locked); err != nil {
				return err
			}
		}
		blobStore, err := rt.BlobStores.Store(leaseCtx, storeName)
		if err != nil {
			return err
		}
		infos = make([]domain.BlobInfo, 0, len(uploads))
		for _, upload := range uploads {
			file, err := openStagedUpload(upload)
			if err != nil {
				return err
			}
			info, putErr := blobStore.Put(leaseCtx, upload.Digest, file)
			_ = file.Close()
			if putErr != nil {
				return fmt.Errorf("persist blob: %w", putErr)
			}
			infos = append(infos, info)
		}
		published := append([]domain.Asset(nil), assets...)
		for index := range published {
			published[index].BlobStore = storeName
		}
		stored, err = rt.blobPlacement().PutAssets(leaseCtx, published)
		return err
	})
	if errors.Is(err, errBlobStoreBindingChanged) ||
		errors.Is(err, ErrBlobStoreLeaseSetChanged) {
		return rt.CommitStagedAssetsValidated(
			ctx, repositoryName, uploads, assets, dependencyStores, validate,
		)
	}
	return infos, stored, err
}
