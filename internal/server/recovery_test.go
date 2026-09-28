package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/store"
)

const (
	recoveryToken    = "recovery-bootstrap-token"
	recoveryRawPath  = "/repository/raw/recovery/artifact.bin"
	recoveryLatePath = "/repository/raw/recovery/after-backup.bin"
)

type recoveryFixture struct {
	handler  *Server
	metadata store.Store
}

type recoveryState struct {
	raw      []byte
	config   []byte
	manifest []byte
	token    string
}

func TestSQLiteFilesystemPairedBackupRestoresThroughServer(t *testing.T) {
	ctx := context.Background()
	sourceDirectory := t.TempDir()
	source := openSQLiteRecoveryServer(t, sourceDirectory)
	state := seedRecoveryServer(t, source.handler)
	stopRecoveryServer(t, source)

	restoredDirectory := t.TempDir()
	copyRecoveryTree(t, sourceDirectory, restoredDirectory)

	// A write acknowledged after the recovery point belongs to a later state.
	// Restoring the paired snapshot must roll metadata and blobs back together.
	source = openSQLiteRecoveryServer(t, sourceDirectory)
	putRecoveryAsset(t, source.handler, recoveryLatePath, []byte("after backup"), recoveryToken)
	stopRecoveryServer(t, source)

	restored := openSQLiteRecoveryServer(t, restoredDirectory)
	defer stopRecoveryServer(t, restored)
	assertRecoveryState(t, ctx, restored.handler, state)
}

func openSQLiteRecoveryServer(t *testing.T, dataDirectory string) recoveryFixture {
	t.Helper()
	metadata, err := store.OpenSQLite(filepath.Join(dataDirectory, "suxen.db"))
	if err != nil {
		t.Fatal(err)
	}
	blobURL := "fs://" + filepath.Join(dataDirectory, "blobs")
	blobStore, err := blob.NewFS(filepath.Join(dataDirectory, "blobs"))
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	return bootstrapRecoveryServer(t, metadata, blobStore, recoveryConfig(dataDirectory, blobURL))
}

func openPostgresRecoveryServer(
	t *testing.T,
	databaseURL string,
	blobURL string,
	blobStore blob.Store,
) recoveryFixture {
	t.Helper()
	metadata, err := store.OpenPostgres(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return bootstrapRecoveryServer(t, metadata, blobStore, recoveryConfig(t.TempDir(), blobURL))
}

func recoveryConfig(dataDirectory string, blobURL string) config.Config {
	return config.Config{
		DataDir:           dataDirectory,
		BlobURL:           blobURL,
		BootstrapUser:     "admin",
		BootstrapPassword: "recovery-password",
		BootstrapToken:    recoveryToken,
		OIDCStateSecret:   "recovery-test-oidc-state-secret",
		MaxUploadBytes:    16 << 20,
	}
}

func bootstrapRecoveryServer(
	t *testing.T,
	metadata store.Store,
	blobStore blob.Store,
	configuration config.Config,
) recoveryFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(configuration, metadata, blobStore, logger)
	if _, err := handler.Bootstrap(context.Background()); err != nil {
		_ = handler.Close()
		_ = metadata.Close()
		t.Fatal(err)
	}
	return recoveryFixture{handler: handler, metadata: metadata}
}

func stopRecoveryServer(t *testing.T, fixture recoveryFixture) {
	t.Helper()
	if err := fixture.handler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.metadata.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedRecoveryServer(t *testing.T, handler *Server) recoveryState {
	t.Helper()
	state := recoveryState{
		raw:    []byte("paired recovery raw artifact"),
		config: []byte("{}"),
	}
	putRecoveryAsset(t, handler, recoveryRawPath, state.raw, recoveryToken)

	configDigest := testDigest(state.config)
	configurationUpload := recoveryRequest(
		t,
		handler,
		http.MethodPost,
		"/repository/oci/v2/recovery/image/blobs/uploads/?digest="+configDigest,
		state.config,
		"application/octet-stream",
		recoveryToken,
	)
	assertStatus(t, configurationUpload, http.StatusCreated)
	configurationUpload.Body.Close()

	state.manifest = []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{
			"mediaType":"application/vnd.oci.image.config.v1+json",
			"digest":%q,
			"size":%d
		},
		"layers":[]
	}`, configDigest, len(state.config)))
	manifest := recoveryRequest(
		t,
		handler,
		http.MethodPut,
		"/repository/oci/v2/recovery/image/manifests/backup",
		state.manifest,
		"application/vnd.oci.image.manifest.v1+json",
		recoveryToken,
	)
	assertStatus(t, manifest, http.StatusCreated)
	manifest.Body.Close()

	role := recoveryRequest(
		t,
		handler,
		http.MethodPost,
		"/api/v1/roles",
		[]byte(`{"name":"recovery-reader","privileges":["repository:raw:read","repository:oci:read"]}`),
		"application/json",
		recoveryToken,
	)
	assertStatus(t, role, http.StatusCreated)
	role.Body.Close()
	user := recoveryRequest(
		t,
		handler,
		http.MethodPost,
		"/api/v1/users",
		[]byte(`{"username":"recovery-reader","password":"recovery-reader-password","admin":false,"roles":["recovery-reader"]}`),
		"application/json",
		recoveryToken,
	)
	assertStatus(t, user, http.StatusCreated)
	user.Body.Close()
	tokenResponse := recoveryRequest(
		t,
		handler,
		http.MethodPost,
		"/api/v1/users/recovery-reader/tokens",
		[]byte(`{"name":"restore-check","scopes":["repository:raw:read","repository:oci:read"]}`),
		"application/json",
		recoveryToken,
	)
	assertStatus(t, tokenResponse, http.StatusCreated)
	var created tokenCreatedResponse
	if err := json.NewDecoder(tokenResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	tokenResponse.Body.Close()
	state.token = created.Token
	if state.token == "" {
		t.Fatal("recovery reader token is empty")
	}
	return state
}

func assertRecoveryState(
	t *testing.T,
	ctx context.Context,
	handler *Server,
	state recoveryState,
) {
	t.Helper()
	for _, endpoint := range []string{"/healthz", "/readyz"} {
		response := recoveryRequest(t, handler, http.MethodGet, endpoint, nil, "", "")
		assertStatus(t, response, http.StatusOK)
		response.Body.Close()
	}

	raw := recoveryRequest(
		t, handler, http.MethodGet, recoveryRawPath, nil, "", state.token,
	)
	assertStatus(t, raw, http.StatusOK)
	assertBody(t, raw, state.raw)

	configDigest := testDigest(state.config)
	configuration := recoveryRequest(
		t,
		handler,
		http.MethodGet,
		"/repository/oci/v2/recovery/image/blobs/"+configDigest,
		nil,
		"",
		state.token,
	)
	assertStatus(t, configuration, http.StatusOK)
	assertBody(t, configuration, state.config)
	manifest := recoveryRequest(
		t,
		handler,
		http.MethodGet,
		"/repository/oci/v2/recovery/image/manifests/backup",
		nil,
		"",
		state.token,
	)
	assertStatus(t, manifest, http.StatusOK)
	assertBody(t, manifest, state.manifest)

	user := recoveryRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/users/recovery-reader",
		nil,
		"",
		recoveryToken,
	)
	assertStatus(t, user, http.StatusOK)
	user.Body.Close()
	repositories := recoveryRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/repositories/raw",
		nil,
		"",
		recoveryToken,
	)
	assertStatus(t, repositories, http.StatusOK)
	repositories.Body.Close()

	late := recoveryRequest(
		t, handler, http.MethodGet, recoveryLatePath, nil, "", recoveryToken,
	)
	assertStatus(t, late, http.StatusNotFound)
	late.Body.Close()

	if err := handler.metadata.Ready(ctx); err != nil {
		t.Fatalf("restored metadata is not ready: %v", err)
	}
}

func putRecoveryAsset(
	t *testing.T,
	handler *Server,
	requestPath string,
	content []byte,
	token string,
) {
	t.Helper()
	response := recoveryRequest(
		t,
		handler,
		http.MethodPut,
		requestPath,
		content,
		"application/octet-stream",
		token,
	)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()
}

func recoveryRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	requestPath string,
	body []byte,
	contentType string,
	token string,
) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func copyRecoveryBlobs(t *testing.T, ctx context.Context, source blob.Store, target blob.Store) {
	t.Helper()
	blobs, err := collectBlobs(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range blobs {
		reader, _, err := source.Get(ctx, info.Digest)
		if err != nil {
			t.Fatal(err)
		}
		_, putErr := target.Put(ctx, info.Digest, reader)
		closeErr := reader.Close()
		if putErr != nil || closeErr != nil {
			t.Fatalf("copy blob %s: put=%v close=%v", info.Digest, putErr, closeErr)
		}
	}
}

func deleteRecoveryBlobs(ctx context.Context, store blob.Store) {
	blobs, err := collectBlobs(ctx, store)
	if err != nil {
		return
	}
	for _, info := range blobs {
		_ = store.Delete(ctx, info.Digest)
	}
}

func postgresDatabaseURL(t *testing.T, dataSourceName string, database string) string {
	t.Helper()
	parsed, err := url.Parse(dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func recoveryS3URL(t *testing.T, rawURL string, suffix string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + path.Join(strings.Trim(parsed.Path, "/"), suffix)
	return parsed.String()
}

func quotePostgresIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func copyRecoveryTree(t *testing.T, source string, target string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, sourcePath)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(targetPath, 0o750)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		input, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		inputCloseErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return inputCloseErr
	}); err != nil {
		t.Fatal(err)
	}
}
