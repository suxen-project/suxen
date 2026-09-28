// Package gcs is a Google Cloud Storage blob store driver.
//
// It speaks the GCS JSON API directly over net/http — object media up- and
// downloads, metadata reads, prefix listing, and object composition for
// upload sessions — authenticating with Google application default
// credentials or an explicit service-account key file. Transient API
// failures (throttling, 5xx, transport errors) are retried with backoff; all
// requests are idempotent or guarded by generation preconditions, so replays
// are safe. The package registers the "gcs" driver on import and only
// depends on the public suxen SPI, so it doubles as the reference for
// out-of-tree blob store plugins.
//
// Configuration URL:
//
//	gcs://bucket/prefix?credentials=/path/to/service-account.json
//
// Query parameters:
//   - credentials: path to a service-account JSON key, or "anonymous" to
//     send unauthenticated requests. Defaults to Google application default
//     credentials (GOOGLE_APPLICATION_CREDENTIALS, workload identity, ...);
//     with a custom endpoint the default is anonymous, matching emulators.
//   - endpoint: override the API endpoint (for example a fake-gcs-server
//     emulator). Defaults to https://storage.googleapis.com.
package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/suxen-project/suxen/spi/blob"
)

const (
	defaultEndpoint = "https://storage.googleapis.com"
	storageScope    = "https://www.googleapis.com/auth/devstorage.read_write"
)

func init() {
	blob.Register(blob.Driver{
		Name:                   "gcs",
		URLScheme:              "gcs",
		Open:                   open,
		CanonicalConfiguration: canonicalConfiguration,
		// Bucket contents are visible to every replica.
		SharedStorage: true,
	})
}

type configuration struct {
	bucket      string
	prefix      string
	endpoint    string
	credentials string
}

// Store implements blob.Store and blob.UploadStore on a GCS bucket.
type Store struct {
	client   *http.Client
	endpoint string
	bucket   string
	prefix   string
}

func open(rawURL string) (blob.Store, error) {
	parsed, err := parseConfiguration(rawURL)
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(parsed)
	if err != nil {
		return nil, err
	}
	return &Store{
		client:   client,
		endpoint: parsed.endpoint,
		bucket:   parsed.bucket,
		prefix:   parsed.prefix,
	}, nil
}

func parseConfiguration(rawURL string) (configuration, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return configuration{}, fmt.Errorf("parse GCS configuration: %w", err)
	}
	if parsed.Scheme != "gcs" {
		return configuration{}, errors.New("GCS blob store configuration must use gcs://")
	}
	if parsed.Host == "" {
		return configuration{}, errors.New("GCS configuration requires a bucket name")
	}
	if parsed.User != nil {
		return configuration{}, errors.New("GCS configuration must not contain user information")
	}

	prefix := strings.Trim(parsed.Path, "/")
	if prefix != "" {
		cleaned := path.Clean(prefix)
		if cleaned != prefix || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return configuration{}, errors.New("GCS prefix must be a clean relative path")
		}
	}

	query := parsed.Query()
	config := configuration{
		bucket:      parsed.Host,
		prefix:      prefix,
		endpoint:    defaultEndpoint,
		credentials: query.Get("credentials"),
	}
	for key := range query {
		if key != "credentials" && key != "endpoint" {
			return configuration{}, fmt.Errorf("unsupported GCS configuration parameter %q", key)
		}
	}
	if endpoint := query.Get("endpoint"); endpoint != "" {
		endpointURL, err := url.Parse(endpoint)
		if err != nil {
			return configuration{}, fmt.Errorf("parse GCS endpoint: %w", err)
		}
		if endpointURL.Scheme != "http" && endpointURL.Scheme != "https" {
			return configuration{}, errors.New("GCS endpoint must use http or https")
		}
		if endpointURL.Host == "" {
			return configuration{}, errors.New("GCS endpoint requires a host")
		}
		config.endpoint = strings.TrimRight(endpoint, "/")
		if config.credentials == "" {
			// Custom endpoints are emulators or gateways that manage their
			// own access; application default credentials would fail hard in
			// environments that have none.
			config.credentials = "anonymous"
		}
	}
	return config, nil
}

func newHTTPClient(config configuration) (*http.Client, error) {
	if config.credentials == "anonymous" {
		return &http.Client{}, nil
	}
	if config.credentials != "" {
		keyData, err := os.ReadFile(config.credentials)
		if err != nil {
			return nil, fmt.Errorf("read GCS credentials file: %w", err)
		}
		credentials, err := google.CredentialsFromJSON(
			context.Background(),
			keyData,
			storageScope,
		)
		if err != nil {
			return nil, fmt.Errorf("parse GCS credentials: %w", err)
		}
		return oauth2.NewClient(context.Background(), credentials.TokenSource), nil
	}
	credentials, err := google.FindDefaultCredentials(context.Background(), storageScope)
	if err != nil {
		return nil, fmt.Errorf("resolve Google application default credentials: %w", err)
	}
	return oauth2.NewClient(context.Background(), credentials.TokenSource), nil
}

// canonicalConfiguration normalizes bucket, prefix, and endpoint while
// excluding credential parameters, so configuration aliases hash to the same
// physical identity without incorporating secrets.
func canonicalConfiguration(rawURL string) (string, error) {
	config, err := parseConfiguration(rawURL)
	if err != nil {
		return "", err
	}
	canonical := "gcs://" + config.bucket
	if config.prefix != "" {
		canonical += "/" + config.prefix
	}
	return canonical + "?endpoint=" + url.QueryEscape(config.endpoint), nil
}

// objectMetadata is the subset of the JSON API object resource the driver
// reads. Size and generation are decimal strings on the wire.
type objectMetadata struct {
	Name       string    `json:"name"`
	Size       string    `json:"size"`
	Generation string    `json:"generation"`
	Updated    time.Time `json:"updated"`
}

func (metadata objectMetadata) sizeBytes() (int64, error) {
	size, err := strconv.ParseInt(metadata.Size, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse GCS object size %q: %w", metadata.Size, err)
	}
	return size, nil
}

var errPreconditionFailed = errors.New("precondition failed")

// Put spools and hashes the stream, then uploads it under a create-only
// precondition so concurrent writers of the same digest stay idempotent.
func (store *Store) Put(
	ctx context.Context,
	expectedDigest string,
	reader io.Reader,
) (blob.Info, error) {
	expectedDigest, err := normalizeDigest(expectedDigest)
	if err != nil {
		return blob.Info{}, err
	}
	staged, err := os.CreateTemp("", "suxen-gcs-upload-*")
	if err != nil {
		return blob.Info{}, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hash), &contextReader{ctx: ctx, r: reader})
	if err != nil {
		return blob.Info{}, err
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		return blob.Info{}, fmt.Errorf(
			"%w: expected %s, got %s",
			blob.ErrDigestMismatch,
			expectedDigest,
			actualDigest,
		)
	}
	if existing, err := store.Head(ctx, expectedDigest); err == nil {
		return existing, nil
	} else if !errors.Is(err, blob.ErrNotFound) {
		return blob.Info{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return blob.Info{}, err
	}

	_, err = store.uploadObject(ctx, store.key(expectedDigest), staged, size, true)
	if errors.Is(err, errPreconditionFailed) {
		// Another writer persisted the same digest first; content addressing
		// makes the results identical.
		return store.Head(ctx, expectedDigest)
	}
	if err != nil {
		return blob.Info{}, fmt.Errorf("put GCS object: %w", err)
	}
	return store.Head(ctx, expectedDigest)
}

// Get opens a GCS object for streaming.
func (store *Store) Get(
	ctx context.Context,
	digest string,
) (io.ReadCloser, blob.Info, error) {
	digest, err := normalizeDigest(digest)
	if err != nil {
		return nil, blob.Info{}, err
	}
	info, err := store.Head(ctx, digest)
	if err != nil {
		return nil, blob.Info{}, err
	}
	response, err := store.do(ctx, http.MethodGet, store.objectURL(store.key(digest))+"?alt=media", nil, "")
	if err != nil {
		return nil, blob.Info{}, err
	}
	return response.Body, info, nil
}

// Head returns GCS object metadata without opening its media stream.
func (store *Store) Head(ctx context.Context, digest string) (blob.Info, error) {
	digest, err := normalizeDigest(digest)
	if err != nil {
		return blob.Info{}, err
	}
	metadata, err := store.objectMetadata(ctx, store.key(digest))
	if err != nil {
		return blob.Info{}, err
	}
	size, err := metadata.sizeBytes()
	if err != nil {
		return blob.Info{}, err
	}
	return blob.Info{
		Digest:     digest,
		Size:       size,
		ModifiedAt: metadata.Updated.UTC(),
	}, nil
}

// Walk yields content-addressed objects page by page.
func (store *Store) Walk(ctx context.Context, yield func(blob.Info) error) error {
	pageToken := ""
	for {
		listURL := store.endpoint + "/storage/v1/b/" + url.PathEscape(store.bucket) +
			"/o?maxResults=1000&fields=" +
			url.QueryEscape("items(name,size,updated),nextPageToken") +
			"&prefix=" + url.QueryEscape(store.contentPrefix())
		if pageToken != "" {
			listURL += "&pageToken=" + url.QueryEscape(pageToken)
		}
		response, err := store.do(ctx, http.MethodGet, listURL, nil, "")
		if err != nil {
			return fmt.Errorf("list GCS objects: %w", err)
		}
		var page struct {
			Items         []objectMetadata `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}
		err = json.NewDecoder(response.Body).Decode(&page)
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("decode GCS object listing: %w", err)
		}
		for _, item := range page.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			digest, err := normalizeDigest(path.Base(item.Name))
			if err != nil || item.Name != store.key(digest) {
				continue
			}
			size, err := item.sizeBytes()
			if err != nil {
				return err
			}
			if err := yield(blob.Info{
				Digest:     digest,
				Size:       size,
				ModifiedAt: item.Updated.UTC(),
			}); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" {
			return ctx.Err()
		}
		pageToken = page.NextPageToken
	}
}

// Delete removes a blob; deleting a missing blob is a no-op.
func (store *Store) Delete(ctx context.Context, digest string) error {
	digest, err := normalizeDigest(digest)
	if err != nil {
		return err
	}
	err = store.deleteObject(ctx, store.key(digest))
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	return err
}

// Ready verifies that the configured bucket accepts a write and delete.
func (store *Store) Ready(ctx context.Context) error {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	probe := path.Join(store.prefix, ".suxen-ready", base64.RawURLEncoding.EncodeToString(random))
	if _, err := store.uploadObject(ctx, probe, bytes.NewReader(nil), 0, false); err != nil {
		return fmt.Errorf("write GCS readiness object: %w", err)
	}
	if err := store.deleteObject(ctx, probe); err != nil {
		return fmt.Errorf("delete GCS readiness object: %w", err)
	}
	return nil
}

// CreateUpload creates an empty transient upload under a create-only
// precondition.
func (store *Store) CreateUpload(ctx context.Context, key string) error {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return err
	}
	// Avoid replacing an existing session even against GCS-compatible gateways
	// that accept, but do not enforce, ifGenerationMatch on media uploads. Real
	// GCS still supplies the create-only precondition below to close the race
	// between this check and the write.
	if _, err := store.objectMetadata(ctx, uploadKey); err == nil {
		return fmt.Errorf("%w: %s", blob.ErrUploadExists, key)
	} else if !errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("check GCS upload: %w", err)
	}
	_, err = store.uploadObject(ctx, uploadKey, bytes.NewReader(nil), 0, true)
	if errors.Is(err, errPreconditionFailed) {
		return fmt.Errorf("%w: %s", blob.ErrUploadExists, key)
	}
	if err != nil {
		return fmt.Errorf("create GCS upload: %w", err)
	}
	return nil
}

// AppendUpload stages the appended bytes as a separate part object, then
// atomically composes session+part guarded by the session's generation. A
// failed staging or compose leaves the previous session content unchanged.
func (store *Store) AppendUpload(
	ctx context.Context,
	key string,
	source io.Reader,
	maxSize int64,
) (int64, error) {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return 0, err
	}
	session, err := store.objectMetadata(ctx, uploadKey)
	if err != nil {
		return 0, err
	}
	initialSize, err := session.sizeBytes()
	if err != nil {
		return 0, err
	}
	if initialSize > maxSize {
		return initialSize, blob.ErrUploadTooLarge
	}

	staged, err := os.CreateTemp("", "suxen-gcs-append-*")
	if err != nil {
		return initialSize, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	remaining := maxSize - initialSize
	readLimit := remaining
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	written, err := io.Copy(
		staged,
		io.LimitReader(&contextReader{ctx: ctx, r: source}, readLimit),
	)
	if err != nil {
		return initialSize, err
	}
	if written > remaining {
		return initialSize, blob.ErrUploadTooLarge
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return initialSize, err
	}

	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return initialSize, err
	}
	partKey := uploadKey + ".part-" + base64.RawURLEncoding.EncodeToString(random)
	if _, err := store.uploadObject(ctx, partKey, staged, written, false); err != nil {
		return initialSize, fmt.Errorf("stage GCS upload append: %w", err)
	}
	defer func() {
		_ = store.deleteObject(context.WithoutCancel(ctx), partKey)
	}()

	if err := store.composeObjects(ctx, uploadKey, partKey, session.Generation); err != nil {
		if errors.Is(err, errPreconditionFailed) {
			return initialSize, errors.New("concurrent GCS upload append detected")
		}
		return initialSize, fmt.Errorf("compose GCS upload append: %w", err)
	}
	return initialSize + written, nil
}

// OpenUpload opens a transient upload for streaming.
func (store *Store) OpenUpload(
	ctx context.Context,
	key string,
) (io.ReadCloser, int64, error) {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return nil, 0, err
	}
	metadata, err := store.objectMetadata(ctx, uploadKey)
	if err != nil {
		return nil, 0, err
	}
	size, err := metadata.sizeBytes()
	if err != nil {
		return nil, 0, err
	}
	response, err := store.do(ctx, http.MethodGet, store.objectURL(uploadKey)+"?alt=media", nil, "")
	if err != nil {
		return nil, 0, err
	}
	return response.Body, size, nil
}

// DeleteUpload removes a transient upload. Missing uploads return ErrNotFound.
func (store *Store) DeleteUpload(ctx context.Context, key string) error {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return err
	}
	return store.deleteObject(ctx, uploadKey)
}

func (store *Store) key(digest string) string {
	hash := strings.TrimPrefix(digest, "sha256:")
	return path.Join(store.prefix, "sha256", hash[:2], hash[2:4], hash)
}

func (store *Store) contentPrefix() string {
	return path.Join(store.prefix, "sha256") + "/"
}

func (store *Store) uploadKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", errors.New("invalid upload key")
	}
	cleaned := path.Clean(key)
	if cleaned != key || cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("invalid upload key")
	}
	return path.Join(store.prefix, ".suxen-uploads", cleaned), nil
}

func (store *Store) objectURL(name string) string {
	return store.endpoint + "/storage/v1/b/" + url.PathEscape(store.bucket) +
		"/o/" + url.PathEscape(name)
}

func (store *Store) objectMetadata(ctx context.Context, name string) (objectMetadata, error) {
	response, err := store.do(ctx, http.MethodGet, store.objectURL(name), nil, "")
	if err != nil {
		return objectMetadata{}, err
	}
	defer response.Body.Close()
	var metadata objectMetadata
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		return objectMetadata{}, fmt.Errorf("decode GCS object metadata: %w", err)
	}
	return metadata, nil
}

func (store *Store) uploadObject(
	ctx context.Context,
	name string,
	content io.Reader,
	size int64,
	createOnly bool,
) (objectMetadata, error) {
	uploadURL := store.endpoint + "/upload/storage/v1/b/" + url.PathEscape(store.bucket) +
		"/o?uploadType=media&name=" + url.QueryEscape(name)
	if createOnly {
		uploadURL += "&ifGenerationMatch=0"
	}
	response, err := store.doWithLength(
		ctx,
		http.MethodPost,
		uploadURL,
		content,
		size,
		"application/octet-stream",
	)
	if err != nil {
		return objectMetadata{}, err
	}
	defer response.Body.Close()
	var metadata objectMetadata
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		return objectMetadata{}, fmt.Errorf("decode GCS upload response: %w", err)
	}
	return metadata, nil
}

func (store *Store) composeObjects(
	ctx context.Context,
	destination string,
	source string,
	destinationGeneration string,
) error {
	composeURL := store.objectURL(destination) + "/compose?ifGenerationMatch=" +
		url.QueryEscape(destinationGeneration)
	body, err := json.Marshal(map[string]any{
		"sourceObjects": []map[string]string{
			{"name": destination},
			{"name": source},
		},
		"destination": map[string]string{"contentType": "application/octet-stream"},
	})
	if err != nil {
		return err
	}
	response, err := store.do(ctx, http.MethodPost, composeURL, bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func (store *Store) deleteObject(ctx context.Context, name string) error {
	response, err := store.do(ctx, http.MethodDelete, store.objectURL(name), nil, "")
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func (store *Store) do(
	ctx context.Context,
	method string,
	requestURL string,
	body io.Reader,
	contentType string,
) (*http.Response, error) {
	return store.doWithLength(ctx, method, requestURL, body, -1, contentType)
}

const (
	requestAttempts   = 3
	retryBackoffFloor = 250 * time.Millisecond
)

// doWithLength performs one API request with bounded retries on transient
// failures and maps error statuses. Callers own the response body of
// successful responses.
//
// Retries require replaying the request body, so only nil and seekable bodies
// (the driver's spool files and in-memory readers) retry; every request the
// driver issues is either idempotent or guarded by a generation precondition,
// which keeps replays safe. A replay after an ambiguously-failed
// preconditioned write can observe its own first attempt as a precondition
// failure; the driver's create-only and compose callers treat that as a
// conflict, which is the conservative outcome.
func (store *Store) doWithLength(
	ctx context.Context,
	method string,
	requestURL string,
	body io.Reader,
	contentLength int64,
	contentType string,
) (*http.Response, error) {
	seeker, seekable := body.(io.Seeker)
	var lastErr error
	for attempt := 0; attempt < requestAttempts; attempt++ {
		if attempt > 0 {
			if body != nil && !seekable {
				break
			}
			if seekable {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
			}
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		response, err := store.attempt(ctx, method, requestURL, body, contentLength, contentType)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !retryableRequestError(err) {
			break
		}
	}
	return nil, lastErr
}

func (store *Store) attempt(
	ctx context.Context,
	method string,
	requestURL string,
	body io.Reader,
	contentLength int64,
	contentType string,
) (*http.Response, error) {
	// The transport closes io.Closer bodies after each attempt, which would
	// break replaying a spooled *os.File; hide the Closer so the caller keeps
	// ownership.
	var requestBody io.Reader
	if body != nil {
		requestBody = replayableBody{Reader: body}
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, requestBody)
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		request.ContentLength = contentLength
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := store.client.Do(request)
	if err != nil {
		return nil, &transientError{cause: err}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return nil, blob.ErrNotFound
	case http.StatusPreconditionFailed:
		return nil, errPreconditionFailed
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	failure := fmt.Errorf(
		"GCS API returned %s: %s",
		response.Status,
		strings.TrimSpace(string(detail)),
	)
	if transientStatus(response.StatusCode) {
		return nil, &transientError{cause: failure}
	}
	return nil, failure
}

// replayableBody hides a body's io.Closer from the HTTP transport.
type replayableBody struct {
	io.Reader
}

// transientError marks failures worth retrying: transport errors and the
// throttling/server statuses the GCS SDKs also retry.
type transientError struct {
	cause error
}

func (err *transientError) Error() string { return err.cause.Error() }
func (err *transientError) Unwrap() error { return err.cause }

func transientStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryableRequestError(err error) bool {
	var transient *transientError
	return errors.As(err, &transient) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

func sleepBackoff(ctx context.Context, attempt int) error {
	timer := time.NewTimer(retryBackoffFloor << (attempt - 1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// normalizeDigest validates a SHA-256 digest and returns its canonical form.
// The server always hands drivers normalized digests; this stays defensive so
// direct SPI consumers get coherent errors.
func normalizeDigest(digest string) (string, error) {
	algorithm, hash, ok := strings.Cut(strings.ToLower(digest), ":")
	if !ok {
		algorithm, hash = "sha256", strings.ToLower(digest)
	}
	if algorithm != "sha256" || len(hash) != 64 {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("invalid digest: %w", err)
	}
	return "sha256:" + hash, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

var _ blob.Store = (*Store)(nil)
var _ blob.UploadStore = (*Store)(nil)
