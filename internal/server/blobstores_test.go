package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

func TestBlobStoreAPIAndRepositoryDriverSelection(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "default"))
	if err != nil {
		t.Fatal(err)
	}

	secondaryRoot := filepath.Join(dataDirectory, "secondary")
	putCreatedRoot := filepath.Join(dataDirectory, "put-created")
	t.Setenv("SUXEN_TEST_SECONDARY_STORE", "tracking://"+secondaryRoot)
	t.Setenv("SUXEN_TEST_SECONDARY_ALIAS", "tracking://"+secondaryRoot)
	t.Setenv("SUXEN_TEST_PUT_CREATED_STORE", "tracking://"+putCreatedRoot)
	var selectedDriver string
	var selectedConfiguration string
	factory := func(driver string, configuration string) (blob.Store, error) {
		selectedDriver = driver
		selectedConfiguration = configuration
		return blob.NewFS(strings.TrimPrefix(configuration, "tracking://"))
	}
	handler := NewWithBlobStoreFactory(
		config.Config{
			DataDir:           dataDirectory,
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "default"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
			MaxUploadBytes:    16 << 20,
		},
		metadata,
		defaultStore,
		factory,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	putCreatedStore := blobStoreRequest(
		t,
		handler,
		http.MethodPut,
		"/api/v1/blob-stores/put-created",
		`{
			"driver": "tracking",
			"configurationRef": {"env": "SUXEN_TEST_PUT_CREATED_STORE"}
		}`,
	)
	if putCreatedStore.Code != http.StatusCreated ||
		putCreatedStore.Header().Get("Location") != "/api/v1/blob-stores/put-created" {
		t.Fatalf(
			"PUT-created blob store status = %d, Location = %q, body = %s",
			putCreatedStore.Code,
			putCreatedStore.Header().Get("Location"),
			putCreatedStore.Body.String(),
		)
	}
	deletedPutStore := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/blob-stores/put-created",
		"",
	)
	if deletedPutStore.Code != http.StatusNoContent {
		t.Fatalf("delete PUT-created blob store status = %d", deletedPutStore.Code)
	}

	createStore := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary",
        "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`)
	if createStore.Code != http.StatusCreated {
		t.Fatalf("create blob store status = %d, body = %s", createStore.Code, createStore.Body.String())
	}
	if selectedDriver != "tracking" || selectedConfiguration != "tracking://"+secondaryRoot {
		t.Fatalf("factory selected driver=%q config=%q", selectedDriver, selectedConfiguration)
	}
	if strings.Contains(createStore.Body.String(), secondaryRoot) {
		t.Fatalf("blob store response leaked resolved configuration: %s", createStore.Body.String())
	}
	listStores := blobStoreRequest(t, handler, http.MethodGet, "/api/v1/blob-stores", "")
	if listStores.Code != http.StatusOK || !strings.Contains(listStores.Body.String(), "secondary") {
		t.Fatalf("list blob stores status = %d, body = %s", listStores.Code, listStores.Body.String())
	}
	var blobStorePage httpx.CollectionPage[domain.BlobStore]
	if err := json.Unmarshal(listStores.Body.Bytes(), &blobStorePage); err != nil {
		t.Fatalf("decode blob store page: %v", err)
	}
	if len(blobStorePage.Items) != 2 || blobStorePage.NextCursor != "" {
		t.Fatalf("blob store page = %+v, want two items and no cursor", blobStorePage)
	}
	getStore := blobStoreRequest(t, handler, http.MethodGet, "/api/v1/blob-stores/secondary", "")
	if getStore.Code != http.StatusOK || strings.Contains(getStore.Body.String(), secondaryRoot) {
		t.Fatalf("get blob store status = %d, body = %s", getStore.Code, getStore.Body.String())
	}
	updateStore := blobStoreRequest(t, handler, http.MethodPut, "/api/v1/blob-stores/secondary", `{
        "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
    }`)
	if updateStore.Code != http.StatusOK {
		t.Fatalf("update blob store status = %d, body = %s", updateStore.Code, updateStore.Body.String())
	}
	aliasStore := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/blob-stores", `{
        "name": "secondary-alias",
        "driver": "tracking",
        "configurationRef": {"env": "SUXEN_TEST_SECONDARY_ALIAS"}
    }`)
	if aliasStore.Code != http.StatusConflict {
		t.Fatalf("aliased blob store status = %d, body = %s", aliasStore.Code, aliasStore.Body.String())
	}
	deleteDefault := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/blob-stores/default",
		"",
	)
	if deleteDefault.Code != http.StatusConflict {
		t.Fatalf("delete default status = %d, body = %s", deleteDefault.Code, deleteDefault.Body.String())
	}

	unknownRepository := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "unknown-store",
        "format": "raw",
        "type": "hosted",
        "blobStore": "missing"
    }`)
	if unknownRepository.Code != http.StatusNotFound {
		t.Fatalf("unknown store status = %d, body = %s", unknownRepository.Code, unknownRepository.Body.String())
	}

	createRepository := blobStoreRequest(t, handler, http.MethodPost, "/api/v1/repositories", `{
        "name": "secondary-repository",
        "format": "raw",
        "type": "hosted",
        "blobStore": "secondary"
    }`)
	if createRepository.Code != http.StatusCreated {
		t.Fatalf("create repository status = %d, body = %s", createRepository.Code, createRepository.Body.String())
	}

	content := []byte("stored on the selected named backend")
	upload := httptest.NewRequest(
		http.MethodPut,
		"/repository/secondary-repository/artifact.bin",
		bytes.NewReader(content),
	)
	upload.Header.Set("Authorization", "Bearer "+testToken)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	digestBytes := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	secondaryStore, err := blob.NewFS(secondaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondaryStore.Head(context.Background(), digest); err != nil {
		t.Fatalf("selected backend does not contain uploaded blob: %v", err)
	}
	if _, err := defaultStore.Head(context.Background(), digest); err == nil {
		t.Fatal("default backend unexpectedly contains named-store upload")
	}
	idempotentStoreUpdate := blobStoreRequest(
		t,
		handler,
		http.MethodPut,
		"/api/v1/blob-stores/secondary",
		`{
            "driver": "tracking",
            "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"}
        }`,
	)
	if idempotentStoreUpdate.Code != http.StatusOK {
		t.Fatalf(
			"idempotent in-use store update status = %d, body = %s",
			idempotentStoreUpdate.Code,
			idempotentStoreUpdate.Body.String(),
		)
	}
	policyUpdate := blobStoreRequest(
		t,
		handler,
		http.MethodPut,
		"/api/v1/blob-stores/secondary",
		`{
            "driver": "tracking",
            "configurationRef": {"env": "SUXEN_TEST_SECONDARY_STORE"},
            "attributes": {
                "uploadSessions": {
                    "staleAfter": "15m",
                    "maxPrincipalSessions": 2
                }
            }
        }`,
	)
	if policyUpdate.Code != http.StatusOK ||
		!strings.Contains(policyUpdate.Body.String(), `"staleAfter":"15m"`) {
		t.Fatalf(
			"in-use store policy update status = %d, body = %s",
			policyUpdate.Code,
			policyUpdate.Body.String(),
		)
	}
	inUseStoreUpdate := blobStoreRequest(
		t,
		handler,
		http.MethodPut,
		"/api/v1/blob-stores/secondary",
		`{
            "driver": "tracking",
            "configurationRef": {"env": "SUXEN_TEST_SECONDARY_ALIAS"}
        }`,
	)
	if inUseStoreUpdate.Code != http.StatusConflict {
		t.Fatalf(
			"in-use store update status = %d, body = %s",
			inUseStoreUpdate.Code,
			inUseStoreUpdate.Body.String(),
		)
	}
	moveRepository := blobStoreRequest(
		t,
		handler,
		http.MethodPut,
		"/api/v1/repositories/secondary-repository",
		`{
            "format": "raw",
            "type": "hosted",
            "blobStore": "default"
        }`,
	)
	if moveRepository.Code != http.StatusConflict {
		t.Fatalf(
			"move nonempty repository status = %d, body = %s",
			moveRepository.Code,
			moveRepository.Body.String(),
		)
	}
	persistedRepository, err := metadata.Repository(
		context.Background(),
		"secondary-repository",
	)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRepository.BlobStore != "secondary" {
		t.Fatalf(
			"failed API move changed blob store to %q",
			persistedRepository.BlobStore,
		)
	}

	inUseDelete := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/blob-stores/secondary",
		"",
	)
	if inUseDelete.Code != http.StatusConflict {
		t.Fatalf("in-use delete status = %d, body = %s", inUseDelete.Code, inUseDelete.Body.String())
	}
	deleteRepository := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/repositories/secondary-repository",
		"",
	)
	if deleteRepository.Code != http.StatusNoContent {
		t.Fatalf("delete repository status = %d", deleteRepository.Code)
	}
	deleteStore := blobStoreRequest(
		t,
		handler,
		http.MethodDelete,
		"/api/v1/blob-stores/secondary",
		"",
	)
	if deleteStore.Code != http.StatusNoContent {
		t.Fatalf("delete blob store status = %d, body = %s", deleteStore.Code, deleteStore.Body.String())
	}
}

func TestBlobStoreValidationDoesNotEchoResolvedConfiguration(t *testing.T) {
	t.Setenv("SUXEN_SECRET_STORE", "credential-must-not-leak")
	manager := newBlobStoreManager(
		nil,
		nil,
		func(_ string, configuration string) (blob.Store, error) {
			return nil, errors.New("rejected " + configuration)
		},
		nil,
	)
	err := manager.Validate(context.Background(), domain.BlobStore{
		Name:   "secret-store",
		Driver: "custom",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_SECRET_STORE",
		},
	})
	if err == nil {
		t.Fatal("validate accepted a rejected driver configuration")
	}
	if strings.Contains(err.Error(), "credential-must-not-leak") {
		t.Fatalf("validation error leaked resolved configuration: %v", err)
	}
}

func TestPhysicalBlobStoreIdentityCanonicalizesBuiltInTargets(t *testing.T) {
	root := t.TempDir()
	firstFilesystem, err := content.PhysicalIdentity("fs", "fs://"+root)
	if err != nil {
		t.Fatal(err)
	}
	secondFilesystem, err := content.PhysicalIdentity("fs", "fs://"+root+"/")
	if err != nil {
		t.Fatal(err)
	}
	if firstFilesystem != secondFilesystem {
		t.Fatalf("filesystem identities differ: %s != %s", firstFilesystem, secondFilesystem)
	}

	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o750); err != nil {
		t.Fatal(err)
	}
	symlinkParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	throughMissingLeaf, err := content.PhysicalIdentity(
		"fs",
		"fs://"+filepath.Join(symlinkParent, "new-store"),
	)
	if err != nil {
		t.Fatal(err)
	}
	throughRealParent, err := content.PhysicalIdentity(
		"fs",
		"fs://"+filepath.Join(realParent, "new-store"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if throughMissingLeaf != throughRealParent {
		t.Fatal("filesystem identity did not resolve an existing symlink parent")
	}

	firstS3, err := content.PhysicalIdentity(
		"s3",
		"s3://ARTIFACTS/prefix/?region=eu-west-1&pathStyle=true",
	)
	if err != nil {
		t.Fatal(err)
	}
	secondS3, err := content.PhysicalIdentity(
		"s3",
		"s3://artifacts/prefix?pathStyle=true&region=eu-west-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstS3 != secondS3 {
		t.Fatalf("S3 identities differ: %s != %s", firstS3, secondS3)
	}
	defaultedS3, err := content.PhysicalIdentity(
		"s3",
		"s3://artifacts/prefix?credential=old&ignored=value",
	)
	if err != nil {
		t.Fatal(err)
	}
	explicitS3, err := content.PhysicalIdentity(
		"s3",
		"s3://artifacts/prefix?region=us-east-1&credential=new",
	)
	if err != nil {
		t.Fatal(err)
	}
	if defaultedS3 != explicitS3 {
		t.Fatal("S3 identity included ignored credentials or omitted runtime defaults")
	}
	repeatedS3, err := content.PhysicalIdentity(
		"s3",
		"s3://artifacts/prefix?region=eu-west-1&region=us-east-1&pathStyle=true",
	)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedS3 != firstS3 {
		t.Fatal("S3 identity did not use the runtime value for a repeated parameter")
	}
	firstCredential, err := content.PhysicalIdentity("custom", "target;credential=one")
	if err != nil {
		t.Fatal(err)
	}
	secondCredential, err := content.PhysicalIdentity("custom", "target;credential=two")
	if err != nil {
		t.Fatal(err)
	}
	if firstCredential == secondCredential {
		t.Fatal("custom driver credentials were omitted from physical identity")
	}
}

func TestPhysicalBlobStoreIdentityRejectsS3TargetTraversal(t *testing.T) {
	tests := []string{
		"s3://artifacts/..",
		"s3://artifacts/repositories/../other",
		"s3://artifacts/content?endpoint=https%3A%2F%2Fminio%2Fa%2F..%2Fb",
	}
	for _, configuration := range tests {
		t.Run(configuration, func(t *testing.T) {
			if _, err := content.PhysicalIdentity("s3", configuration); err == nil {
				t.Fatalf("configuration %q produced an ambiguous identity", configuration)
			}
		})
	}
}

func TestBlobStoreManagerRejectsAmbiguousS3ConfigurationRotation(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	const original = "s3://artifacts/content?endpoint=https%3A%2F%2Fminio%2Fb"
	t.Setenv("SUXEN_S3_ROTATION", original)
	identity, err := content.PhysicalIdentity("s3", original)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "archive",
		Driver: "s3",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_S3_ROTATION",
		},
		PhysicalIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}

	var factoryCalls atomic.Int64
	manager := newBlobStoreManager(metadata, nil, func(string, string) (blob.Store, error) {
		factoryCalls.Add(1)
		return newBlockingBlobStore(), nil
	}, nil)
	if _, err := manager.Store(context.Background(), "archive"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(
		"SUXEN_S3_ROTATION",
		"s3://artifacts/content?endpoint=https%3A%2F%2Fminio%2Fa%2F..%2Fb",
	)
	if _, err := manager.Store(context.Background(), "archive"); err == nil {
		t.Fatal("ambiguous S3 target rotation bypassed validation")
	}
	if calls := factoryCalls.Load(); calls != 1 {
		t.Fatalf("factory calls = %d, want no open for rejected rotation", calls)
	}
	manager.Forget("archive")
}

func TestBlobStoreManagerDefersCloseUntilActiveOperationsFinish(t *testing.T) {
	tests := []struct {
		name      string
		replace   bool
		operation func(context.Context, blob.Store) error
	}{
		{
			name:    "put during replacement",
			replace: true,
			operation: func(ctx context.Context, store blob.Store) error {
				_, err := store.Put(ctx, strings.Repeat("0", 64), strings.NewReader("content"))
				return err
			},
		},
		{
			name: "walk during deletion",
			operation: func(ctx context.Context, store blob.Store) error {
				return store.Walk(ctx, func(domain.BlobInfo) error { return nil })
			},
		},
		{
			name:    "delete during replacement",
			replace: true,
			operation: func(ctx context.Context, store blob.Store) error {
				return store.Delete(ctx, strings.Repeat("0", 64))
			},
		},
		{
			name: "readiness during deletion",
			operation: func(ctx context.Context, store blob.Store) error {
				return store.Ready(ctx)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newBlobStoreManager(nil, nil, nil, nil)
			backing := newBlockingBlobStore()
			resource := domain.BlobStore{Name: "archive", Driver: "test"}
			managed := manager.Remember(resource, backing)

			operationDone := make(chan error, 1)
			go func() {
				operationDone <- test.operation(context.Background(), managed)
			}()
			<-backing.started

			if test.replace {
				manager.Remember(resource, newBlockingBlobStore())
			} else {
				manager.Forget(resource.Name)
			}
			if backing.closed.Load() {
				t.Fatal("backing store closed while an operation was active")
			}
			close(backing.proceed)
			if err := <-operationDone; err != nil {
				t.Fatal(err)
			}
			if !backing.closed.Load() {
				t.Fatal("retired backing store was not closed after the operation")
			}
			if backing.observedClosed.Load() {
				t.Fatal("operation observed a closed backing store")
			}
		})
	}
}

func TestBlobStoreManagerHoldsLeaseForStreamedReader(t *testing.T) {
	manager := newBlobStoreManager(nil, nil, nil, nil)
	backing := newBlockingBlobStore()
	close(backing.proceed)
	resource := domain.BlobStore{Name: "archive", Driver: "test"}
	managed := manager.Remember(resource, backing)

	reader, _, err := managed.Get(context.Background(), strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	manager.Forget(resource.Name)
	if backing.closed.Load() {
		t.Fatal("backing store closed while a streamed reader was active")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !backing.closed.Load() {
		t.Fatal("retired backing store was not closed after its reader")
	}
}

type blockingBlobStore struct {
	started        chan struct{}
	proceed        chan struct{}
	startOnce      sync.Once
	closed         atomic.Bool
	observedClosed atomic.Bool
}

func newBlockingBlobStore() *blockingBlobStore {
	return &blockingBlobStore{
		started: make(chan struct{}),
		proceed: make(chan struct{}),
	}
}

func (store *blockingBlobStore) wait() {
	if store.closed.Load() {
		store.observedClosed.Store(true)
	}
	store.startOnce.Do(func() { close(store.started) })
	<-store.proceed
	if store.closed.Load() {
		store.observedClosed.Store(true)
	}
}

func (store *blockingBlobStore) Put(
	context.Context,
	string,
	io.Reader,
) (domain.BlobInfo, error) {
	store.wait()
	return domain.BlobInfo{}, nil
}

func (store *blockingBlobStore) Get(
	context.Context,
	string,
) (io.ReadCloser, domain.BlobInfo, error) {
	store.startOnce.Do(func() { close(store.started) })
	return io.NopCloser(strings.NewReader("content")), domain.BlobInfo{}, nil
}

func (store *blockingBlobStore) Head(context.Context, string) (domain.BlobInfo, error) {
	store.wait()
	return domain.BlobInfo{}, nil
}

func (store *blockingBlobStore) Walk(context.Context, func(domain.BlobInfo) error) error {
	store.wait()
	return nil
}

func (store *blockingBlobStore) Delete(context.Context, string) error {
	store.wait()
	return nil
}

func (store *blockingBlobStore) Ready(context.Context) error {
	store.wait()
	return nil
}

func (store *blockingBlobStore) Close() error {
	store.closed.Store(true)
	return nil
}

func TestBlobStoreAPIRejectsAliasedFilesystemAndS3Targets(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "default"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithBlobStoreFactory(
		config.Config{
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "default"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
		},
		metadata,
		defaultStore,
		func(driver string, configuration string) (blob.Store, error) {
			if driver == "fs" {
				return blob.NewFS(strings.TrimPrefix(configuration, "fs://"))
			}
			return defaultStore, nil
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	filesystemRoot := filepath.Join(dataDirectory, "shared")
	t.Setenv("SUXEN_FS_FIRST", "fs://"+filesystemRoot)
	t.Setenv("SUXEN_FS_SECOND", "fs://"+filesystemRoot+"/")
	t.Setenv(
		"SUXEN_S3_FIRST",
		"s3://ARTIFACTS/prefix/?region=eu-west-1&pathStyle=true",
	)
	t.Setenv(
		"SUXEN_S3_SECOND",
		"s3://artifacts/prefix?pathStyle=true&region=eu-west-1",
	)

	tests := []struct {
		name           string
		driver         string
		environment    string
		expectedStatus int
	}{
		{name: "fs-first", driver: "fs", environment: "SUXEN_FS_FIRST", expectedStatus: http.StatusCreated},
		{name: "fs-second", driver: "fs", environment: "SUXEN_FS_SECOND", expectedStatus: http.StatusConflict},
		{name: "s3-first", driver: "s3", environment: "SUXEN_S3_FIRST", expectedStatus: http.StatusCreated},
		{name: "s3-second", driver: "s3", environment: "SUXEN_S3_SECOND", expectedStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"name":%q,"driver":%q,"configurationRef":{"env":%q}}`,
				test.name,
				test.driver,
				test.environment,
			)
			response := blobStoreRequest(
				t,
				handler,
				http.MethodPost,
				"/api/v1/blob-stores",
				body,
			)
			if response.Code != test.expectedStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestBlobStoreManagerCoalescesOpensAndDetectsReferenceRotation(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "default"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dataDirectory, "secondary")
	configuration := "tracking://" + root
	t.Setenv("SUXEN_COALESCED_STORE", configuration)
	identity, err := content.PhysicalIdentity("tracking", configuration)
	if err != nil {
		t.Fatal(err)
	}
	var factoryCalls atomic.Int64
	handler := NewWithBlobStoreFactory(
		config.Config{
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "default"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
		},
		metadata,
		defaultStore,
		func(_ string, configuration string) (blob.Store, error) {
			factoryCalls.Add(1)
			time.Sleep(25 * time.Millisecond)
			return blob.NewFS(strings.TrimPrefix(configuration, "tracking://"))
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "coalesced",
		Driver: "tracking",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_COALESCED_STORE",
		},
		PhysicalIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}

	const callers = 16
	var waitGroup sync.WaitGroup
	errorsByCaller := make(chan error, callers)
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, err := handler.blobStores.Store(context.Background(), "coalesced")
			errorsByCaller <- err
		}()
	}
	waitGroup.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := factoryCalls.Load(); calls != 1 {
		t.Fatalf("factory calls = %d, want 1", calls)
	}

	t.Setenv("SUXEN_COALESCED_STORE", "tracking://"+filepath.Join(dataDirectory, "rotated"))
	if _, err := handler.blobStores.Store(
		context.Background(),
		"coalesced",
	); !errors.Is(err, domain.ErrBlobStoreConfigChanged) {
		t.Fatalf("rotated configuration error = %v", err)
	}
}

func TestBlobStoreManagerOpeningOutlivesFirstCaller(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	configuration := "tracking://" + filepath.Join(dataDirectory, "archive")
	t.Setenv("SUXEN_CANCELLATION_STORE", configuration)
	identity, err := content.PhysicalIdentity("tracking", configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "archive",
		Driver: "tracking",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_CANCELLATION_STORE",
		},
		PhysicalIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}

	blockingMetadata := &blockingBlobStoreMetadata{
		Store:   metadata,
		started: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	manager := newBlobStoreManager(
		blockingMetadata,
		nil,
		func(string, string) (blob.Store, error) {
			return newBlockingBlobStore(), nil
		},
		nil,
	)

	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := manager.Store(firstContext, "archive")
		firstResult <- err
	}()
	<-blockingMetadata.started

	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error = %v, want context cancellation", err)
	}

	secondContext := &observedContext{
		Context: context.Background(),
		waiting: make(chan struct{}),
	}
	secondResult := make(chan error, 1)
	go func() {
		_, err := manager.Store(secondContext, "archive")
		secondResult <- err
	}()
	<-secondContext.waiting
	close(blockingMetadata.proceed)
	if err := <-secondResult; err != nil {
		t.Fatalf("second caller inherited cancellation: %v", err)
	}
	manager.Forget("archive")
	if calls := blockingMetadata.calls.Load(); calls != 1 {
		t.Fatalf("metadata opens = %d, want one shared open", calls)
	}
}

type observedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *observedContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

type blockingBlobStoreMetadata struct {
	store.Store
	started   chan struct{}
	proceed   chan struct{}
	startOnce sync.Once
	calls     atomic.Int64
}

func (metadata *blockingBlobStoreMetadata) BlobStore(
	ctx context.Context,
	name string,
) (domain.BlobStore, error) {
	metadata.calls.Add(1)
	metadata.startOnce.Do(func() { close(metadata.started) })
	select {
	case <-ctx.Done():
		return domain.BlobStore{}, ctx.Err()
	case <-metadata.proceed:
		return metadata.Store.BlobStore(ctx, name)
	}
}

func TestBootstrapCreatesDefaultWithoutPersistingConfiguration(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	secretBearingConfiguration := "s3://bucket?access_key=must-not-persist"
	t.Setenv("SUXEN_BLOBSTORE", secretBearingConfiguration)
	handler := NewWithBlobStoreFactory(
		config.Config{
			BlobURL:           secretBearingConfiguration,
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
		},
		metadata,
		defaultStore,
		func(string, string) (blob.Store, error) { return defaultStore, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	adopted, err := metadata.BlobStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Driver != "s3" || adopted.ConfigurationRef == nil ||
		adopted.ConfigurationRef.Env != "SUXEN_BLOBSTORE" {
		t.Fatalf("adopted default blob store = %+v", adopted)
	}
	encoded, err := json.Marshal(adopted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-persist") {
		t.Fatalf("adopted resource leaked configuration: %s", encoded)
	}

}

func TestBootstrapUsesOpenedDefaultWhenEnvironmentUsesBuiltInDefault(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUXEN_BLOBSTORE", "")
	handler := NewWithBlobStoreFactory(
		config.Config{
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "blobs"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
		},
		metadata,
		defaultStore,
		func(string, string) (blob.Store, error) {
			t.Fatal("built-in default should use the already-open store")
			return nil, nil
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	resolved, err := handler.blobStores.Store(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if !content.WrapsBacking(resolved, defaultStore) {
		t.Fatal("default blob store did not preserve the already-open backend")
	}
}

func TestGarbageCollectionScopesIdenticalDigestsByBlobStore(t *testing.T) {
	dataDirectory := t.TempDir()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	defaultStore, err := blob.NewFS(filepath.Join(dataDirectory, "default"))
	if err != nil {
		t.Fatal(err)
	}
	secondaryRoot := filepath.Join(dataDirectory, "secondary")
	t.Setenv("SUXEN_GC_SECONDARY", "tracking://"+secondaryRoot)
	handler := NewWithBlobStoreFactory(
		config.Config{
			DataDir:           dataDirectory,
			BlobURL:           "fs://" + filepath.Join(dataDirectory, "default"),
			BootstrapUser:     "admin",
			BootstrapPassword: "test-password",
			BootstrapToken:    testToken,
		},
		metadata,
		defaultStore,
		func(_ string, configuration string) (blob.Store, error) {
			return blob.NewFS(strings.TrimPrefix(configuration, "tracking://"))
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondaryIdentity, err := content.PhysicalIdentity(
		"tracking",
		"tracking://"+secondaryRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateBlobStore(context.Background(), domain.BlobStore{
		Name:   "secondary",
		Driver: "tracking",
		ConfigurationRef: &domain.ConfigurationReference{
			Env: "SUXEN_GC_SECONDARY",
		},
		PhysicalIdentity: secondaryIdentity,
	}); err != nil {
		t.Fatal(err)
	}

	content := []byte("same digest, independent physical lifecycle")
	digestBytes := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	if _, err := defaultStore.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: "raw",
		Path:       "retained.bin",
		Digest:     digest,
		Size:       int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	secondaryStore, err := blob.NewFS(secondaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondaryStore.Put(context.Background(), digest, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	result, err := handler.collectGarbage(
		context.Background(),
		false,
		0,
		time.Now().Add(time.Second),
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("garbage collection result = %+v", result)
	}
	if _, err := defaultStore.Head(context.Background(), digest); err != nil {
		t.Fatalf("referenced default-store blob was deleted: %v", err)
	}
	if _, err := secondaryStore.Head(context.Background(), digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreferenced secondary-store blob error = %v, want not found", err)
	}
}

func blobStoreRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// newBlobStoreManager builds a store manager over a possibly nil metadata
// store and metrics, which the production constructor does not need to allow.
func newBlobStoreManager(
	metadata store.Store,
	defaultStore blob.Store,
	factory content.BlobStoreFactory,
	metrics *serverMetrics,
) *content.StoreManager {
	var metricsImpl content.Metrics
	if metrics != nil {
		metricsImpl = metrics
	}
	return content.NewStoreManager(metadata, defaultStore, factory, metricsImpl)
}
