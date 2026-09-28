//go:build suxen_integration

package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestS3StoreIntegration(t *testing.T) {
	rawURL := os.Getenv("SUXEN_TEST_S3")
	if rawURL == "" {
		t.Skip("SUXEN_TEST_S3 is not configured")
	}
	store, err := NewS3(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client, ok := store.client.(*s3.Client)
	if !ok {
		t.Fatalf("S3 client has type %T", store.client)
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(store.bucket),
	})
	if err != nil {
		var apiError interface{ ErrorCode() string }
		if !errors.As(err, &apiError) || apiError.ErrorCode() != "BucketAlreadyOwnedByYou" {
			t.Fatal(err)
		}
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	content := []byte("S3 content-addressed integration")
	digest := "sha256:9c1279368b0bb0bceb6397b55fa904bc71627a2de04b77a494f1207922cb5788"
	if _, err := store.Put(ctx, digest, bytes.NewReader([]byte("wrong"))); !errors.Is(err, domain.ErrDigestMismatch) {
		t.Fatalf("digest mismatch returned %v", err)
	}
	info, err := store.Put(ctx, digest, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("got S3 object size %d", info.Size)
	}
	reader, downloaded, err := store.Get(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, content) || downloaded.Digest != digest {
		t.Fatalf("unexpected S3 content %q metadata %+v", actual, downloaded)
	}
	var blobs []domain.BlobInfo
	err = store.Walk(ctx, func(info domain.BlobInfo) error {
		blobs = append(blobs, info)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsBlobDigest(blobs, digest) {
		t.Fatalf("S3 listing did not contain %s: %+v", digest, blobs)
	}
	if err := store.Delete(ctx, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted S3 object returned %v, want ErrNotFound", err)
	}

	const uploadKey = "oci/integration/session"
	if err := store.CreateUpload(ctx, uploadKey); err != nil {
		t.Fatal(err)
	}
	if size, err := store.AppendUpload(ctx, uploadKey, strings.NewReader("chunk"), 16); err != nil || size != 5 {
		t.Fatalf("append S3 upload: size=%d err=%v", size, err)
	}
	uploadReader, uploadSize, err := store.OpenUpload(ctx, uploadKey)
	if err != nil {
		t.Fatal(err)
	}
	uploadContent, readErr := io.ReadAll(uploadReader)
	closeErr := uploadReader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read S3 upload: read=%v close=%v", readErr, closeErr)
	}
	if uploadSize != 5 || string(uploadContent) != "chunk" {
		t.Fatalf("S3 upload: size=%d content=%q", uploadSize, uploadContent)
	}
	if err := store.DeleteUpload(ctx, uploadKey); err != nil {
		t.Fatal(err)
	}
}
