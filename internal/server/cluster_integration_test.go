//go:build suxen_integration

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/store"
)

func TestPostgresS3ReplicasShareStateAndSurviveReplicaLoss(t *testing.T) {
	postgresURL := os.Getenv("SUXEN_TEST_POSTGRES")
	s3URL := os.Getenv("SUXEN_TEST_S3")
	if postgresURL == "" || s3URL == "" {
		t.Skip("SUXEN_TEST_POSTGRES and SUXEN_TEST_S3 are not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const replicaCount = 3
	suffix := fmt.Sprint(time.Now().UnixNano())
	bootstrapToken := "cluster-token-" + suffix
	replicas := make([]*Server, 0, replicaCount)
	metadataStores := make([]*store.Postgres, 0, replicaCount)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for replica := 0; replica < replicaCount; replica++ {
		metadata, err := store.OpenPostgres(postgresURL)
		if err != nil {
			t.Fatal(err)
		}
		metadataStores = append(metadataStores, metadata)
		t.Cleanup(func() {
			_ = metadata.Close()
		})

		blobStore, err := blob.NewS3(s3URL)
		if err != nil {
			t.Fatal(err)
		}
		replicas = append(replicas, New(config.Config{
			BlobURL:           s3URL,
			BootstrapUser:     "cluster-admin-" + suffix,
			BootstrapPassword: "cluster-password-" + suffix,
			BootstrapToken:    bootstrapToken,
			MaxUploadBytes:    16 << 20,
		}, metadata, blobStore, logger))
	}

	bootstrapReplicasConcurrently(t, ctx, replicas)

	content := []byte("shared content from a three-replica deployment")
	assetPath := "/repository/raw/cluster/" + suffix + ".txt"
	upload := clusterRequest(
		replicas[0],
		http.MethodPut,
		assetPath,
		content,
		bootstrapToken,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()

	// Closing one replica's independent database pool models abrupt pod loss. The
	// remaining replicas must continue from the shared PostgreSQL and S3 state.
	if err := metadataStores[0].Close(); err != nil {
		t.Fatal(err)
	}
	download := clusterRequest(
		replicas[1],
		http.MethodGet,
		assetPath,
		nil,
		bootstrapToken,
	)
	assertStatus(t, download, http.StatusOK)
	assertBody(t, download, content)

	now := time.Now().UTC()
	leaseName := "cluster-integration-" + suffix
	acquired, err := metadataStores[1].AcquireLease(
		ctx,
		leaseName,
		"replica-2",
		now,
		now.Add(time.Second),
	)
	if err != nil || !acquired {
		t.Fatalf("first replica lease: acquired=%v err=%v", acquired, err)
	}
	acquired, err = metadataStores[2].AcquireLease(
		ctx,
		leaseName,
		"replica-3",
		now,
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("a second replica acquired an active leader lease")
	}
	acquired, err = metadataStores[2].AcquireLease(
		ctx,
		leaseName,
		"replica-3",
		now.Add(2*time.Second),
		now.Add(3*time.Second),
	)
	if err != nil || !acquired {
		t.Fatalf("leader takeover: acquired=%v err=%v", acquired, err)
	}
}

func bootstrapReplicasConcurrently(
	t *testing.T,
	ctx context.Context,
	replicas []*Server,
) {
	t.Helper()

	errorsByReplica := make([]error, len(replicas))
	var waitGroup sync.WaitGroup
	for index, replica := range replicas {
		waitGroup.Add(1)
		go func(replicaIndex int, replicaServer *Server) {
			defer waitGroup.Done()
			_, errorsByReplica[replicaIndex] = replicaServer.Bootstrap(ctx)
		}(index, replica)
	}
	waitGroup.Wait()

	for index, err := range errorsByReplica {
		if err != nil {
			t.Fatalf("bootstrap replica %d: %v", index+1, err)
		}
	}
}

func clusterRequest(
	handler http.Handler,
	method string,
	requestPath string,
	body []byte,
	token string,
) *http.Response {
	request := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
