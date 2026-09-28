package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestParseS3Configuration(t *testing.T) {
	configuration, err := parseS3Configuration(
		"s3://artifacts/repositories/content?region=eu-west-1&" +
			"endpoint=http%3A%2F%2Fminio%3A9000&pathStyle=true",
	)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.bucket != "artifacts" {
		t.Fatalf("got bucket %q", configuration.bucket)
	}
	if configuration.prefix != "repositories/content" {
		t.Fatalf("got prefix %q", configuration.prefix)
	}
	if configuration.region != "eu-west-1" {
		t.Fatalf("got region %q", configuration.region)
	}
	if configuration.endpoint != "http://minio:9000" {
		t.Fatalf("got endpoint %q", configuration.endpoint)
	}
	if !configuration.usePathStyle {
		t.Fatal("path-style addressing was not enabled")
	}
}

func TestParseS3ConfigurationRejectsURLCredentials(t *testing.T) {
	if _, err := parseS3Configuration("s3://access:secret@artifacts/content"); err == nil {
		t.Fatal("S3 URL credentials were accepted")
	}
}

func TestParseS3ConfigurationCanonicalizesRuntimeTarget(t *testing.T) {
	configuration, err := parseS3Configuration(
		"s3://ARTIFACTS/repositories//content/?" +
			"endpoint=https%3A%2F%2FMINIO%2Fapi%2F%2Fv1%2F",
	)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.bucket != "artifacts" {
		t.Fatalf("bucket = %q, want artifacts", configuration.bucket)
	}
	if configuration.prefix != "repositories/content" {
		t.Fatalf("prefix = %q, want repositories/content", configuration.prefix)
	}
	if configuration.endpoint != "https://minio/api/v1" {
		t.Fatalf("endpoint = %q, want https://minio/api/v1", configuration.endpoint)
	}
}

func TestParseS3ConfigurationRejectsTargetTraversal(t *testing.T) {
	tests := []string{
		"s3://artifacts/..",
		"s3://artifacts/repositories/../other",
		"s3://artifacts/content?endpoint=https%3A%2F%2Fminio%2Fa%2F..%2Fb",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := parseS3Configuration(rawURL); err == nil {
				t.Fatalf("configuration %q accepted target traversal", rawURL)
			}
		})
	}
}

func TestS3KeyUsesContentAddressedLayout(t *testing.T) {
	store := &S3{bucket: "artifacts", prefix: "repositories"}
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	expected := "repositories/sha256/01/23/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if actual := store.key(digest); actual != expected {
		t.Fatalf("S3 key = %q, want %q", actual, expected)
	}
}

func TestS3PutUsesMultipartUploadForLargeBlob(t *testing.T) {
	const finalPartSize = 123
	uploadSize := int64(2*manager.MinUploadPartSize + finalPartSize)
	digest := digestForRepeatedByte(t, 'x', uploadSize)

	api := newMultipartS3API()
	uploader := manager.NewUploader(api, func(uploader *manager.Uploader) {
		uploader.PartSize = manager.MinUploadPartSize
		uploader.Concurrency = 2
	})
	store := &S3{
		client:   api,
		uploader: uploader,
		bucket:   "artifacts",
		prefix:   "repositories",
	}

	info, err := store.Put(
		context.Background(),
		digest,
		io.LimitReader(repeatingByteReader('x'), uploadSize),
	)
	if err != nil {
		t.Fatal(err)
	}
	if info.Digest != digest || info.Size != uploadSize {
		t.Fatalf("uploaded blob info = %+v", info)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.putCalls != 0 {
		t.Fatalf("PutObject called %d times for a multipart upload", api.putCalls)
	}
	if api.createCalls != 1 || api.completeCalls != 1 {
		t.Fatalf(
			"multipart calls: create=%d complete=%d",
			api.createCalls,
			api.completeCalls,
		)
	}
	partSizes := make([]int64, 0, len(api.parts))
	for _, size := range api.parts {
		partSizes = append(partSizes, size)
	}
	sort.Slice(partSizes, func(left, right int) bool {
		return partSizes[left] < partSizes[right]
	})
	expectedPartSizes := []int64{
		finalPartSize,
		manager.MinUploadPartSize,
		manager.MinUploadPartSize,
	}
	if !slices.Equal(partSizes, expectedPartSizes) {
		t.Fatalf("uploaded part sizes = %v, want %v", partSizes, expectedPartSizes)
	}
	if api.bucket != store.bucket || api.key != store.key(digest) {
		t.Fatalf("multipart target = %s/%s", api.bucket, api.key)
	}
	if api.contentType != "application/octet-stream" {
		t.Fatalf("multipart content type = %q", api.contentType)
	}
}

func TestS3PutSkipsUploadWhenBlobExists(t *testing.T) {
	content := []byte("already present")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	api := newMultipartS3API()
	api.completed = true
	api.size = int64(len(content))
	store := &S3{
		client:   api,
		uploader: manager.NewUploader(api),
		bucket:   "artifacts",
	}

	info, err := store.Put(context.Background(), digest, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if info.Digest != digest || info.Size != int64(len(content)) {
		t.Fatalf("existing blob info = %+v", info)
	}
	if api.putCalls != 0 || api.createCalls != 0 {
		t.Fatalf(
			"existing blob triggered upload: put=%d create=%d",
			api.putCalls,
			api.createCalls,
		)
	}
}

func TestStageS3UploadAppendRejectsOversizedDataWithoutAStagedFile(t *testing.T) {
	staged, size, err := stageS3UploadAppend(
		context.Background(),
		strings.NewReader("first"),
		5,
		strings.NewReader("-too-long"),
		10,
	)
	if staged != nil {
		staged.Close()
		os.Remove(staged.Name())
		t.Fatal("oversized append returned a staged file")
	}
	if size != 5 || !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("oversized append: size=%d err=%v", size, err)
	}
}

func TestStageS3UploadAppendProducesCombinedContent(t *testing.T) {
	staged, size, err := stageS3UploadAppend(
		context.Background(),
		strings.NewReader("first-"),
		6,
		strings.NewReader("second"),
		12,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	content, err := io.ReadAll(staged)
	if err != nil {
		t.Fatal(err)
	}
	if size != 12 || string(content) != "first-second" {
		t.Fatalf("staged append: size=%d content=%q", size, content)
	}
}

type repeatingByteReader byte

func (reader repeatingByteReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte(reader)
	}
	return len(buffer), nil
}

func digestForRepeatedByte(t *testing.T, value byte, size int64) string {
	t.Helper()
	hash := sha256.New()
	if _, err := io.CopyN(hash, repeatingByteReader(value), size); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

type multipartS3API struct {
	mu            sync.Mutex
	bucket        string
	key           string
	contentType   string
	parts         map[int32]int64
	size          int64
	completed     bool
	putCalls      int
	createCalls   int
	completeCalls int
}

func newMultipartS3API() *multipartS3API {
	return &multipartS3API{parts: make(map[int32]int64)}
}

func (api *multipartS3API) PutObject(
	_ context.Context,
	_ *s3.PutObjectInput,
	_ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.putCalls++
	return nil, errors.New("unexpected single-part upload")
}

func (api *multipartS3API) CreateMultipartUpload(
	_ context.Context,
	input *s3.CreateMultipartUploadInput,
	_ ...func(*s3.Options),
) (*s3.CreateMultipartUploadOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.createCalls++
	api.bucket = aws.ToString(input.Bucket)
	api.key = aws.ToString(input.Key)
	api.contentType = aws.ToString(input.ContentType)
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (api *multipartS3API) UploadPart(
	_ context.Context,
	input *s3.UploadPartInput,
	_ ...func(*s3.Options),
) (*s3.UploadPartOutput, error) {
	size, err := io.Copy(io.Discard, input.Body)
	if err != nil {
		return nil, err
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	api.parts[aws.ToInt32(input.PartNumber)] = size
	return &s3.UploadPartOutput{
		ETag: aws.String(fmt.Sprintf("part-%d", aws.ToInt32(input.PartNumber))),
	}, nil
}

func (api *multipartS3API) CompleteMultipartUpload(
	_ context.Context,
	input *s3.CompleteMultipartUploadInput,
	_ ...func(*s3.Options),
) (*s3.CompleteMultipartUploadOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.completeCalls++
	api.completed = true
	for _, part := range api.parts {
		api.size += part
	}
	return &s3.CompleteMultipartUploadOutput{
		Bucket: input.Bucket,
		Key:    input.Key,
	}, nil
}

func (api *multipartS3API) AbortMultipartUpload(
	_ context.Context,
	_ *s3.AbortMultipartUploadInput,
	_ ...func(*s3.Options),
) (*s3.AbortMultipartUploadOutput, error) {
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (api *multipartS3API) HeadObject(
	_ context.Context,
	_ *s3.HeadObjectInput,
	_ ...func(*s3.Options),
) (*s3.HeadObjectOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.completed {
		return nil, &smithy.GenericAPIError{Code: "NotFound", Message: "not found"}
	}
	modifiedAt := time.Unix(1, 0).UTC()
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(api.size),
		LastModified:  &modifiedAt,
	}, nil
}

func (api *multipartS3API) GetObject(
	_ context.Context,
	_ *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	return nil, errors.New("unexpected GetObject call")
}

func (api *multipartS3API) ListObjectsV2(
	_ context.Context,
	_ *s3.ListObjectsV2Input,
	_ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	return nil, errors.New("unexpected ListObjectsV2 call")
}

func (api *multipartS3API) DeleteObject(
	_ context.Context,
	_ *s3.DeleteObjectInput,
	_ ...func(*s3.Options),
) (*s3.DeleteObjectOutput, error) {
	return nil, errors.New("unexpected DeleteObject call")
}

var _ s3API = (*multipartS3API)(nil)

func containsBlobDigest(blobs []domain.BlobInfo, expected string) bool {
	for _, blob := range blobs {
		if blob.Digest == expected {
			return true
		}
	}
	return false
}
