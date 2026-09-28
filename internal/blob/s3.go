package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/suxen-project/suxen/internal/domain"
)

const (
	// Three 16 MiB buffers balance throughput with predictable per-upload memory.
	s3UploadPartSize    = 16 * 1024 * 1024
	s3UploadConcurrency = 3
)

type s3Configuration struct {
	bucket       string
	prefix       string
	region       string
	endpoint     string
	usePathStyle bool
}

// CanonicalS3Configuration returns the effective S3 configuration used by the
// runtime driver. Unsupported query parameters are intentionally omitted, and
// defaulted options are made explicit, so aliases hash to the same physical
// identity without incorporating credentials from unrelated query parameters.
func CanonicalS3Configuration(rawURL string) (string, error) {
	configuration, err := parseS3Configuration(rawURL)
	if err != nil {
		return "", err
	}

	prefix := ""
	if configuration.prefix != "" {
		prefix = "/" + configuration.prefix
	}

	query := url.Values{}
	query.Set("region", configuration.region)
	query.Set("pathStyle", strconv.FormatBool(configuration.usePathStyle))
	if configuration.endpoint != "" {
		query.Set("endpoint", configuration.endpoint)
	}
	canonical := &url.URL{
		Scheme:   "s3",
		Host:     strings.ToLower(configuration.bucket),
		Path:     prefix,
		RawQuery: query.Encode(),
	}
	return canonical.String(), nil
}

func canonicalS3Endpoint(rawEndpoint string) (string, error) {
	if rawEndpoint == "" {
		return "", nil
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || endpoint.Host == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", fmt.Errorf("S3 endpoint must be an absolute HTTP URL")
	}
	if endpoint.User != nil {
		return "", fmt.Errorf("S3 endpoint credentials are not allowed")
	}
	if containsPathTraversal(endpoint.Path) {
		return "", fmt.Errorf("S3 endpoint path cannot contain '.' or '..' segments")
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	endpoint.Host = strings.ToLower(endpoint.Host)
	endpoint.Fragment = ""
	endpoint.RawQuery = endpoint.Query().Encode()
	endpoint.RawPath = ""
	endpoint.Path = strings.TrimSuffix(path.Clean("/"+strings.Trim(endpoint.Path, "/")), "/")
	if endpoint.Path == "/." {
		endpoint.Path = ""
	}
	return endpoint.String(), nil
}

func canonicalS3Prefix(rawPath string) (string, error) {
	if containsPathTraversal(rawPath) {
		return "", fmt.Errorf("S3 prefix cannot contain '.' or '..' segments")
	}
	prefix := strings.TrimPrefix(path.Clean("/"+strings.Trim(rawPath, "/")), "/")
	return prefix, nil
}

func containsPathTraversal(rawPath string) bool {
	for _, segment := range strings.Split(rawPath, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// S3 stores immutable blobs in Amazon S3 or an API-compatible object store.
type S3 struct {
	client   s3API
	uploader s3Uploader
	bucket   string
	prefix   string
}

// s3API describes the object operations used by the S3 blob store and its
// multipart uploader.
type s3API interface {
	manager.UploadAPIClient
	s3.ListObjectsV2APIClient

	DeleteObject(
		context.Context,
		*s3.DeleteObjectInput,
		...func(*s3.Options),
	) (*s3.DeleteObjectOutput, error)
	GetObject(
		context.Context,
		*s3.GetObjectInput,
		...func(*s3.Options),
	) (*s3.GetObjectOutput, error)
	HeadObject(
		context.Context,
		*s3.HeadObjectInput,
		...func(*s3.Options),
	) (*s3.HeadObjectOutput, error)
}

// s3Uploader uploads a staged object using either a single request or S3's
// multipart protocol, depending on the object's size.
type s3Uploader interface {
	Upload(
		context.Context,
		*s3.PutObjectInput,
		...func(*manager.Uploader),
	) (*manager.UploadOutput, error)
}

// NewS3 creates an S3-compatible blob store from an s3:// URL.
//
// The URL host is the bucket, the path is an optional key prefix, and supported
// query parameters are region, endpoint, and pathStyle. Credentials use the
// standard AWS SDK credential chain and are never accepted in the URL.
func NewS3(rawURL string) (*S3, error) {
	configuration, err := parseS3Configuration(rawURL)
	if err != nil {
		return nil, err
	}
	loaded, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(configuration.region),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(loaded, func(options *s3.Options) {
		options.UsePathStyle = configuration.usePathStyle
		if configuration.endpoint != "" {
			options.BaseEndpoint = aws.String(configuration.endpoint)
		}
	})
	return &S3{
		client: client,
		uploader: manager.NewUploader(client, func(uploader *manager.Uploader) {
			uploader.PartSize = s3UploadPartSize
			uploader.Concurrency = s3UploadConcurrency
		}),
		bucket: configuration.bucket,
		prefix: configuration.prefix,
	}, nil
}

func parseS3Configuration(rawURL string) (s3Configuration, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return s3Configuration{}, fmt.Errorf("parse S3 URL: %w", err)
	}
	if parsed.Scheme != "s3" || parsed.Host == "" {
		return s3Configuration{}, fmt.Errorf("S3 URL must use s3://bucket[/prefix]")
	}
	if parsed.User != nil {
		return s3Configuration{}, fmt.Errorf("S3 credentials must use the AWS credential chain")
	}
	prefix, err := canonicalS3Prefix(parsed.Path)
	if err != nil {
		return s3Configuration{}, err
	}
	endpoint, err := canonicalS3Endpoint(parsed.Query().Get("endpoint"))
	if err != nil {
		return s3Configuration{}, err
	}
	configuration := s3Configuration{
		bucket:   strings.ToLower(parsed.Host),
		prefix:   prefix,
		region:   parsed.Query().Get("region"),
		endpoint: endpoint,
	}
	if configuration.region == "" {
		configuration.region = "us-east-1"
	}
	pathStyleValue := parsed.Query().Get("pathStyle")
	if pathStyleValue == "" {
		configuration.usePathStyle = configuration.endpoint != ""
	} else {
		configuration.usePathStyle, err = strconv.ParseBool(pathStyleValue)
		if err != nil {
			return s3Configuration{}, fmt.Errorf("parse S3 pathStyle: %w", err)
		}
	}
	return configuration, nil
}

// Put verifies and uploads one immutable blob.
func (store *S3) Put(
	ctx context.Context,
	expectedDigest string,
	reader io.Reader,
) (domain.BlobInfo, error) {
	expectedDigest, err := NormalizeDigest(expectedDigest)
	if err != nil {
		return domain.BlobInfo{}, err
	}
	staged, err := os.CreateTemp("", "suxen-s3-upload-*")
	if err != nil {
		return domain.BlobInfo{}, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hash), &contextReader{ctx: ctx, r: reader})
	if err != nil {
		return domain.BlobInfo{}, err
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		return domain.BlobInfo{}, fmt.Errorf(
			"%w: expected %s, got %s",
			domain.ErrDigestMismatch,
			expectedDigest,
			actualDigest,
		)
	}
	if existing, err := store.Head(ctx, expectedDigest); err == nil {
		return existing, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.BlobInfo{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return domain.BlobInfo{}, err
	}
	_, err = store.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(store.bucket),
		Key:           aws.String(store.key(expectedDigest)),
		Body:          staged,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return domain.BlobInfo{}, fmt.Errorf("put S3 object: %w", err)
	}
	return store.Head(ctx, expectedDigest)
}

// Get opens an S3 object for streaming.
func (store *S3) Get(
	ctx context.Context,
	digest string,
) (io.ReadCloser, domain.BlobInfo, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return nil, domain.BlobInfo{}, err
	}
	output, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(store.key(digest)),
	})
	if err != nil {
		return nil, domain.BlobInfo{}, mapS3Error(err)
	}
	return output.Body, domain.BlobInfo{
		Digest:     digest,
		Size:       aws.ToInt64(output.ContentLength),
		ModifiedAt: aws.ToTime(output.LastModified).UTC(),
	}, nil
}

// Head returns S3 object metadata without opening its body.
func (store *S3) Head(ctx context.Context, digest string) (domain.BlobInfo, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return domain.BlobInfo{}, err
	}
	output, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(store.key(digest)),
	})
	if err != nil {
		return domain.BlobInfo{}, mapS3Error(err)
	}
	return domain.BlobInfo{
		Digest:     digest,
		Size:       aws.ToInt64(output.ContentLength),
		ModifiedAt: aws.ToTime(output.LastModified).UTC(),
	}, nil
}

// Walk yields valid content-addressed objects page by page.
func (store *S3) Walk(ctx context.Context, yield func(domain.BlobInfo) error) error {
	paginator := s3.NewListObjectsV2Paginator(store.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(store.bucket),
		Prefix: aws.String(store.contentPrefix()),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list S3 objects: %w", err)
		}
		for _, object := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			objectKey := aws.ToString(object.Key)
			digest, err := NormalizeDigest(path.Base(objectKey))
			if err != nil || objectKey != store.key(digest) {
				continue
			}
			if err := yield(domain.BlobInfo{
				Digest:     digest,
				Size:       aws.ToInt64(object.Size),
				ModifiedAt: aws.ToTime(object.LastModified).UTC(),
			}); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// Delete removes one content-addressed S3 object.
func (store *S3) Delete(ctx context.Context, digest string) error {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return err
	}
	_, err = store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(store.key(digest)),
	})
	if err != nil {
		return fmt.Errorf("delete S3 object: %w", err)
	}
	return nil
}

// CreateUpload creates an empty transient upload in the S3 store.
func (store *S3) CreateUpload(ctx context.Context, key string) error {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return err
	}
	_, err = store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(store.bucket),
		Key:           aws.String(uploadKey),
		Body:          bytes.NewReader(nil),
		ContentLength: aws.Int64(0),
		ContentType:   aws.String("application/octet-stream"),
		IfNoneMatch:   aws.String("*"),
	})
	if isS3PreconditionFailed(err) {
		return fmt.Errorf("%w: %s", ErrUploadExists, key)
	}
	if err != nil {
		return fmt.Errorf("create S3 upload: %w", err)
	}
	return nil
}

func isS3PreconditionFailed(err error) bool {
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed"
}

// AppendUpload atomically replaces a transient S3 upload with its appended content.
func (store *S3) AppendUpload(
	ctx context.Context,
	key string,
	source io.Reader,
	maxSize int64,
) (int64, error) {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return 0, err
	}
	existing, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(uploadKey),
	})
	if err != nil {
		return 0, mapS3Error(err)
	}
	defer existing.Body.Close()

	initialSize := aws.ToInt64(existing.ContentLength)
	if initialSize > maxSize {
		return initialSize, ErrUploadTooLarge
	}
	staged, finalSize, err := stageS3UploadAppend(
		ctx,
		existing.Body,
		initialSize,
		source,
		maxSize,
	)
	if err != nil {
		return initialSize, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	_, err = store.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(store.bucket),
		Key:           aws.String(uploadKey),
		Body:          staged,
		ContentLength: aws.Int64(finalSize),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return initialSize, fmt.Errorf("append S3 upload: %w", err)
	}
	return finalSize, nil
}

// OpenUpload opens a transient S3 upload for streaming.
func (store *S3) OpenUpload(
	ctx context.Context,
	key string,
) (io.ReadCloser, int64, error) {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return nil, 0, err
	}
	output, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(uploadKey),
	})
	if err != nil {
		return nil, 0, mapS3Error(err)
	}
	return output.Body, aws.ToInt64(output.ContentLength), nil
}

// DeleteUpload removes a transient S3 upload.
func (store *S3) DeleteUpload(ctx context.Context, key string) error {
	uploadKey, err := store.uploadKey(key)
	if err != nil {
		return err
	}
	if _, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(uploadKey),
	}); err != nil {
		return mapS3Error(err)
	}
	_, err = store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(uploadKey),
	})
	if err != nil {
		return fmt.Errorf("delete S3 upload: %w", err)
	}
	return nil
}

func stageS3UploadAppend(
	ctx context.Context,
	existing io.Reader,
	initialSize int64,
	source io.Reader,
	maxSize int64,
) (*os.File, int64, error) {
	staged, err := os.CreateTemp("", "suxen-s3-session-*")
	if err != nil {
		return nil, 0, err
	}
	failed := func(operationErr error) (*os.File, int64, error) {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
		return nil, initialSize, operationErr
	}

	copiedExisting, err := io.Copy(
		staged,
		&contextReader{ctx: ctx, r: io.LimitReader(existing, limitPlusOne(initialSize))},
	)
	if err != nil {
		return failed(err)
	}
	if copiedExisting != initialSize {
		return failed(fmt.Errorf("S3 upload size changed while reading"))
	}

	remaining := maxSize - initialSize
	written, err := io.Copy(
		staged,
		io.LimitReader(&contextReader{ctx: ctx, r: source}, limitPlusOne(remaining)),
	)
	if err != nil {
		return failed(err)
	}
	if written > remaining {
		return failed(ErrUploadTooLarge)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return failed(err)
	}
	return staged, initialSize + written, nil
}

// Ready verifies that the configured bucket accepts a write and delete.
func (store *S3) Ready(ctx context.Context) error {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	key := path.Join(store.prefix, ".suxen-ready", base64.RawURLEncoding.EncodeToString(random))
	_, err := store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(store.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(nil),
		ContentLength: aws.Int64(0),
	})
	if err != nil {
		return fmt.Errorf("write S3 readiness object: %w", err)
	}
	_, deleteErr := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(key),
	})
	if deleteErr != nil {
		return fmt.Errorf("delete S3 readiness object: %w", deleteErr)
	}
	return nil
}

func (store *S3) key(digest string) string {
	hash := strings.TrimPrefix(digest, "sha256:")
	return path.Join(store.prefix, "sha256", hash[:2], hash[2:4], hash)
}

func (store *S3) contentPrefix() string {
	return path.Join(store.prefix, "sha256") + "/"
}

func (store *S3) uploadKey(key string) (string, error) {
	cleaned, err := cleanUploadKey(key)
	if err != nil {
		return "", err
	}
	return path.Join(store.prefix, ".suxen-uploads", cleaned), nil
}

func mapS3Error(err error) error {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return domain.ErrNotFound
		}
	}
	return err
}

var _ Store = (*S3)(nil)
var _ UploadStore = (*S3)(nil)
