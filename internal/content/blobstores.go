package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	spiblob "github.com/suxen-project/suxen/spi/blob"
	"golang.org/x/sync/singleflight"
)

// BlobStoreFactory constructs a compiled-in blob store driver from its resolved
// configuration. Production servers use the compile-time registry; the explicit type
// also permits deterministic driver-selection tests without global registration.
type BlobStoreFactory func(driver string, configuration string) (blob.Store, error)

type cachedBlobStore struct {
	key   string
	store *managedBlobStore
}

// managedBlobStore keeps an opened store alive while operations are in flight.
// Retirement prevents new operations and closes the backing store after the last
// active operation (or streamed reader) releases its lease.
type managedBlobStore struct {
	mu       sync.Mutex
	store    blob.Store
	metrics  Metrics
	active   int
	retired  bool
	closeNow sync.Once
}

var errBlobStoreRetired = errors.New("blob store is no longer active")

type StoreManager struct {
	metadata BlobStoreMetadata
	Factory  BlobStoreFactory
	metrics  Metrics
	opened   blob.Store

	mu     sync.Mutex
	stores map[string]cachedBlobStore
	opens  singleflight.Group
}

// BlobStoreMetadata is the only metadata capability needed to open a store.
type BlobStoreMetadata interface {
	BlobStore(context.Context, string) (domain.BlobStore, error)
}

func NewStoreManager(
	metadata BlobStoreMetadata,
	defaultStore blob.Store,
	factory BlobStoreFactory,
	metrics Metrics,
) *StoreManager {
	return &StoreManager{
		metadata: metadata,
		Factory:  factory,
		metrics:  metrics,
		opened:   defaultStore,
		stores:   make(map[string]cachedBlobStore),
	}
}

// SetMetadata replaces the metadata store resources are resolved from.
func (manager *StoreManager) SetMetadata(metadata BlobStoreMetadata) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.metadata = metadata
}

func RegistryFactory(driver string, configuration string) (blob.Store, error) {
	registered, found := spiblob.Lookup(driver)
	if !found {
		return nil, fmt.Errorf("unknown blob store driver %q", driver)
	}
	return registered.Open(configuration)
}

func (manager *StoreManager) Store(
	ctx context.Context,
	name string,
) (blob.Store, error) {
	if name == "" {
		name = "default"
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Each caller observes its own cancellation. The shared open is detached from
	// the first caller so one disconnected request cannot fail all waiters.
	resultChannel := manager.opens.DoChan(name, func() (any, error) {
		return manager.openStore(context.WithoutCancel(ctx), name)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultChannel:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.(blob.Store), nil
	}
}

func (manager *StoreManager) openStore(
	ctx context.Context,
	name string,
) (blob.Store, error) {
	resource, err := manager.meta().BlobStore(ctx, name)
	if err != nil {
		return nil, err
	}
	configuration, resolveErr := resolveBlobStoreConfiguration(resource)
	if resolveErr == nil {
		identity, err := PhysicalIdentity(resource.Driver, configuration)
		if err != nil {
			return nil, err
		}
		if identity != resource.PhysicalIdentity {
			return nil, domain.ErrBlobStoreConfigChanged
		}
	} else if name != "default" || manager.opened == nil {
		return nil, resolveErr
	}

	key := blobStoreCacheKey(resource)
	manager.mu.Lock()
	cached, found := manager.stores[name]
	manager.mu.Unlock()
	if found && cached.key == key {
		return cached.store, nil
	}
	if name == "default" && manager.opened != nil {
		return manager.Remember(resource, manager.opened), nil
	}
	if resolveErr != nil {
		return nil, resolveErr
	}
	opened, err := manager.Factory(resource.Driver, configuration)
	if err != nil {
		return nil, fmt.Errorf(
			"open blob store %q with driver %q: %w",
			resource.Name,
			resource.Driver,
			domain.ErrInvalidBlobStoreConfig,
		)
	}
	if opened == nil {
		return nil, fmt.Errorf(
			"open blob store %q with driver %q: %w",
			resource.Name,
			resource.Driver,
			domain.ErrInvalidBlobStoreConfig,
		)
	}

	return manager.Remember(resource, opened), nil
}

func (manager *StoreManager) Validate(
	ctx context.Context,
	resource domain.BlobStore,
) error {
	_, opened, err := manager.Prepare(ctx, resource)
	closeBlobStore(opened)
	return err
}

func (manager *StoreManager) Prepare(
	ctx context.Context,
	resource domain.BlobStore,
) (domain.BlobStore, blob.Store, error) {
	resource, configuration, err := ResolveResource(resource)
	if err != nil {
		return domain.BlobStore{}, nil, err
	}
	return manager.CheckReady(ctx, resource, configuration)
}

func (manager *StoreManager) CheckReady(
	ctx context.Context,
	resource domain.BlobStore,
	configuration string,
) (domain.BlobStore, blob.Store, error) {
	opened, err := manager.Factory(resource.Driver, configuration)
	if err != nil {
		return domain.BlobStore{}, nil, fmt.Errorf(
			"blob store %q driver %q rejected its referenced configuration: %w",
			resource.Name,
			resource.Driver,
			domain.ErrInvalidBlobStoreConfig,
		)
	}
	if opened == nil {
		return domain.BlobStore{}, nil, fmt.Errorf(
			"blob store %q driver %q returned no store: %w",
			resource.Name,
			resource.Driver,
			domain.ErrInvalidBlobStoreConfig,
		)
	}
	if err := opened.Ready(ctx); err != nil {
		if manager.metrics != nil {
			manager.metrics.ObserveBlobOperation("ready", err)
		}
		closeBlobStore(opened)
		return domain.BlobStore{}, nil, fmt.Errorf(
			"blob store %q readiness check failed: %w",
			resource.Name,
			err,
		)
	}
	if manager.metrics != nil {
		manager.metrics.ObserveBlobOperation("ready", nil)
	}
	return resource, opened, nil
}

func ResolveResource(
	resource domain.BlobStore,
) (domain.BlobStore, string, error) {
	resource.PhysicalIdentity = strings.Repeat("0", 64)
	if err := resource.Validate(); err != nil {
		return domain.BlobStore{}, "", err
	}
	configuration, err := resolveBlobStoreConfiguration(resource)
	if err != nil {
		return domain.BlobStore{}, "", err
	}
	resource.PhysicalIdentity, err = PhysicalIdentity(
		resource.Driver,
		configuration,
	)
	if err != nil {
		return domain.BlobStore{}, "", err
	}
	if err := resource.Validate(); err != nil {
		return domain.BlobStore{}, "", err
	}
	return resource, configuration, nil
}

func (manager *StoreManager) Forget(name string) {
	manager.mu.Lock()
	removed := manager.stores[name]
	delete(manager.stores, name)
	manager.mu.Unlock()
	if removed.store != nil {
		removed.store.retire()
	}
}

func (manager *StoreManager) meta() BlobStoreMetadata {
	return manager.metadata
}

func (manager *StoreManager) Remember(
	resource domain.BlobStore,
	opened blob.Store,
) blob.Store {
	manager.mu.Lock()
	previous := manager.stores[resource.Name]
	if previous.store != nil && previous.store.wraps(opened) {
		manager.stores[resource.Name] = cachedBlobStore{
			key:   blobStoreCacheKey(resource),
			store: previous.store,
		}
		manager.mu.Unlock()
		return previous.store
	}
	managed := newManagedBlobStore(opened, manager.metrics)
	manager.stores[resource.Name] = cachedBlobStore{
		key:   blobStoreCacheKey(resource),
		store: managed,
	}
	manager.mu.Unlock()
	if previous.store != nil && previous.store != managed {
		previous.store.retire()
	}
	return managed
}

func (store *managedBlobStore) wraps(backing blob.Store) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.retired || reflect.TypeOf(store.store) != reflect.TypeOf(backing) {
		return false
	}
	storeType := reflect.TypeOf(store.store)
	return storeType != nil && storeType.Comparable() && store.store == backing
}

func newManagedBlobStore(store blob.Store, metrics Metrics) *managedBlobStore {
	return &managedBlobStore{store: store, metrics: metrics}
}

func (store *managedBlobStore) acquire() (blob.Store, func(), error) {
	store.mu.Lock()
	if store.retired {
		store.mu.Unlock()
		return nil, nil, errBlobStoreRetired
	}
	store.active++
	backing := store.store
	store.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(store.release)
	}
	return backing, release, nil
}

func (store *managedBlobStore) release() {
	store.mu.Lock()
	store.active--
	shouldClose := store.retired && store.active == 0
	store.mu.Unlock()
	if shouldClose {
		store.close()
	}
}

func (store *managedBlobStore) retire() {
	store.mu.Lock()
	store.retired = true
	shouldClose := store.active == 0
	store.mu.Unlock()
	if shouldClose {
		store.close()
	}
}

func (store *managedBlobStore) close() {
	store.closeNow.Do(func() {
		closeBlobStore(store.store)
	})
}

func (store *managedBlobStore) Put(
	ctx context.Context,
	digest string,
	reader io.Reader,
) (info domain.BlobInfo, err error) {
	defer func() {
		store.observe("put", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return domain.BlobInfo{}, err
	}
	defer release()
	return backing.Put(ctx, digest, reader)
}

func (store *managedBlobStore) Get(
	ctx context.Context,
	digest string,
) (reader io.ReadCloser, info domain.BlobInfo, err error) {
	defer func() {
		store.observe("get", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return nil, domain.BlobInfo{}, err
	}
	reader, info, err = backing.Get(ctx, digest)
	if err != nil {
		release()
		return nil, domain.BlobInfo{}, err
	}
	return &leasedBlobReader{ReadCloser: reader, release: release}, info, nil
}

func (store *managedBlobStore) Head(
	ctx context.Context,
	digest string,
) (info domain.BlobInfo, err error) {
	defer func() {
		store.observe("head", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return domain.BlobInfo{}, err
	}
	defer release()
	return backing.Head(ctx, digest)
}

func (store *managedBlobStore) Walk(
	ctx context.Context,
	yield func(domain.BlobInfo) error,
) (err error) {
	defer func() {
		store.observe("walk", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return err
	}
	defer release()
	return backing.Walk(ctx, yield)
}

func (store *managedBlobStore) Delete(ctx context.Context, digest string) (err error) {
	defer func() {
		store.observe("delete", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return err
	}
	defer release()
	return backing.Delete(ctx, digest)
}

func (store *managedBlobStore) CreateUpload(ctx context.Context, key string) (err error) {
	defer func() {
		store.observe("create_upload", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return err
	}
	defer release()
	uploads, ok := backing.(blob.UploadStore)
	if !ok {
		return blob.ErrUploadStoreUnsupported
	}
	return uploads.CreateUpload(ctx, key)
}

func (store *managedBlobStore) AppendUpload(
	ctx context.Context,
	key string,
	source io.Reader,
	maxSize int64,
) (size int64, err error) {
	defer func() {
		store.observe("append_upload", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return 0, err
	}
	defer release()
	uploads, ok := backing.(blob.UploadStore)
	if !ok {
		return 0, blob.ErrUploadStoreUnsupported
	}
	return uploads.AppendUpload(ctx, key, source, maxSize)
}

func (store *managedBlobStore) OpenUpload(
	ctx context.Context,
	key string,
) (reader io.ReadCloser, size int64, err error) {
	defer func() {
		store.observe("open_upload", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return nil, 0, err
	}
	uploads, ok := backing.(blob.UploadStore)
	if !ok {
		release()
		return nil, 0, blob.ErrUploadStoreUnsupported
	}
	reader, size, err = uploads.OpenUpload(ctx, key)
	if err != nil {
		release()
		return nil, 0, err
	}
	return &leasedBlobReader{ReadCloser: reader, release: release}, size, nil
}

func (store *managedBlobStore) DeleteUpload(ctx context.Context, key string) (err error) {
	defer func() {
		store.observe("delete_upload", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return err
	}
	defer release()
	uploads, ok := backing.(blob.UploadStore)
	if !ok {
		return blob.ErrUploadStoreUnsupported
	}
	return uploads.DeleteUpload(ctx, key)
}

func (store *managedBlobStore) Ready(ctx context.Context) (err error) {
	defer func() {
		store.observe("ready", err)
	}()
	backing, release, err := store.acquire()
	if err != nil {
		return err
	}
	defer release()
	return backing.Ready(ctx)
}

func (store *managedBlobStore) observe(operation string, operationErr error) {
	if store.metrics != nil {
		store.metrics.ObserveBlobOperation(operation, operationErr)
	}
}

type leasedBlobReader struct {
	io.ReadCloser
	release func()
}

var _ blob.UploadStore = (*managedBlobStore)(nil)

func (reader *leasedBlobReader) Close() error {
	err := reader.ReadCloser.Close()
	reader.release()
	return err
}

func WrapsBacking(opened blob.Store, backing blob.Store) bool {
	managed, ok := opened.(*managedBlobStore)
	return ok && managed.wraps(backing)
}

func CloseStore(store blob.Store) {
	closeBlobStore(store)
}

func closeBlobStore(store blob.Store) {
	if closer, ok := store.(io.Closer); ok && closer != nil {
		_ = closer.Close()
	}
}

func blobStoreCacheKey(resource domain.BlobStore) string {
	key := resource.Driver + "\x00" + resource.PhysicalIdentity
	if resource.ConfigurationRef != nil {
		key += "\x00" + resource.ConfigurationRef.Env + "\x00" + resource.ConfigurationRef.File
	}
	return key
}

func PhysicalIdentity(driver string, configuration string) (string, error) {
	canonical, err := canonicalBlobStoreConfiguration(driver, configuration)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(driver + "\x00" + canonical))
	return hex.EncodeToString(digest[:]), nil
}

// canonicalBlobStoreConfiguration normalizes a configuration through the
// driver's CanonicalConfiguration capability so physical-identity collision
// detection works for any registered driver. Drivers outside the registry
// (deterministic test factories) fall back to the trimmed configuration.
func canonicalBlobStoreConfiguration(driver string, configuration string) (string, error) {
	if registered, found := spiblob.Lookup(driver); found &&
		registered.CanonicalConfiguration != nil {
		return registered.CanonicalConfiguration(configuration)
	}
	if strings.TrimSpace(configuration) == "" {
		return "", domain.ErrBlobStoreConfigRequired
	}
	return strings.TrimSpace(configuration), nil
}

func resolveBlobStoreConfiguration(resource domain.BlobStore) (string, error) {
	if resource.ConfigurationRef == nil {
		return "", domain.ErrBlobStoreConfigRequired
	}
	if resource.ConfigurationRef.Env != "" {
		value, found := os.LookupEnv(resource.ConfigurationRef.Env)
		if !found || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf(
				"blob store configuration environment variable %s is empty",
				resource.ConfigurationRef.Env,
			)
		}
		return strings.TrimSpace(value), nil
	}
	contents, err := os.ReadFile(resource.ConfigurationRef.File)
	if err != nil {
		return "", fmt.Errorf("read blob store configuration file: %w", err)
	}
	configuration := strings.TrimSpace(string(contents))
	if configuration == "" {
		return "", errors.New("blob store configuration file is empty")
	}
	return configuration, nil
}
