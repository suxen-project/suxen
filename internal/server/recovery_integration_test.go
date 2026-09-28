//go:build suxen_integration

package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
)

func TestPostgresS3PairedBackupRestoresThroughServer(t *testing.T) {
	postgresURL := os.Getenv("SUXEN_TEST_POSTGRES")
	s3URL := os.Getenv("SUXEN_TEST_S3")
	if postgresURL == "" || s3URL == "" {
		t.Skip("SUXEN_TEST_POSTGRES and SUXEN_TEST_S3 are not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	sourceDatabase := "suxen_recovery_source_" + suffix
	restoredDatabase := "suxen_recovery_restored_" + suffix
	adminURL := postgresDatabaseURL(t, postgresURL, "postgres")
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	for _, database := range []string{sourceDatabase, restoredDatabase} {
		database := database
		t.Cleanup(func() {
			_, _ = admin.ExecContext(
				context.Background(),
				"DROP DATABASE IF EXISTS "+quotePostgresIdentifier(database)+" WITH (FORCE)",
			)
		})
	}
	if _, err := admin.ExecContext(
		ctx,
		"CREATE DATABASE "+quotePostgresIdentifier(sourceDatabase),
	); err != nil {
		t.Fatal(err)
	}

	sourceS3URL := recoveryS3URL(t, s3URL, "recovery/source-"+suffix)
	restoredS3URL := recoveryS3URL(t, s3URL, "recovery/restored-"+suffix)
	sourceStore, err := blob.NewS3(sourceS3URL)
	if err != nil {
		t.Fatal(err)
	}
	restoredStore, err := blob.NewS3(restoredS3URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deleteRecoveryBlobs(context.Background(), sourceStore)
		deleteRecoveryBlobs(context.Background(), restoredStore)
	})

	sourceURL := postgresDatabaseURL(t, postgresURL, sourceDatabase)
	restoredURL := postgresDatabaseURL(t, postgresURL, restoredDatabase)
	source := openPostgresRecoveryServer(t, sourceURL, sourceS3URL, sourceStore)
	state := seedRecoveryServer(t, source.handler)
	stopRecoveryServer(t, source)

	// With every application connection closed, clone the PostgreSQL recovery
	// point and copy every committed content-addressed object to a distinct S3
	// prefix. This models the paired database/object snapshots an operator takes
	// while all Suxen replicas are stopped.
	if _, err := admin.ExecContext(
		ctx,
		"CREATE DATABASE "+quotePostgresIdentifier(restoredDatabase)+
			" TEMPLATE "+quotePostgresIdentifier(sourceDatabase),
	); err != nil {
		t.Fatal(err)
	}
	copyRecoveryBlobs(t, ctx, sourceStore, restoredStore)

	source = openPostgresRecoveryServer(t, sourceURL, sourceS3URL, sourceStore)
	putRecoveryAsset(t, source.handler, recoveryLatePath, []byte("after backup"), recoveryToken)
	stopRecoveryServer(t, source)

	restored := openPostgresRecoveryServer(t, restoredURL, restoredS3URL, restoredStore)
	defer stopRecoveryServer(t, restored)
	assertRecoveryState(t, ctx, restored.handler, state)
}
