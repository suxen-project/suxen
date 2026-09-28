package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// A blob store's definition is immutable at the persistence layer, not only in
// the Go mutation path: a raw configuration change that bypasses UpdateBlobStore
// must still be rejected by the schema trigger on both dialects, while an
// attribute-only write is left alone.
func TestBlobStoreDefinitionTriggerRejectsRawChangeOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		serial := time.Now().UnixNano()
		name := fmt.Sprintf("immutable-%d", serial)
		if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
			Name:             name,
			Driver:           "fs",
			PhysicalIdentity: fmt.Sprintf("%064x", serial),
			ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_IMMUTABLE_SRC"},
		}); err != nil {
			t.Fatal(err)
		}

		_, err := metadata.db.ExecContext(
			ctx,
			`UPDATE blob_stores SET driver = ? WHERE name = ?`,
			"s3", name,
		)
		if err == nil {
			t.Fatal("raw driver change was not rejected by the schema trigger")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "definition immutable") {
			t.Fatalf("raw driver change error = %v", err)
		}

		if _, err := metadata.db.ExecContext(
			ctx,
			`UPDATE blob_stores SET physical_identity = ? WHERE name = ?`,
			fmt.Sprintf("%064x", serial+1), name,
		); err == nil {
			t.Fatal("raw physical identity change was not rejected by the schema trigger")
		}

		if _, err := metadata.db.ExecContext(
			ctx,
			`UPDATE blob_stores SET attributes = ? WHERE name = ?`,
			`{"note":"ok"}`, name,
		); err != nil {
			t.Fatalf("attribute-only raw write rejected: %v", err)
		}

		stored, err := metadata.BlobStore(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Driver != "fs" || stored.ConfigurationRef == nil ||
			stored.ConfigurationRef.Env != "SUXEN_IMMUTABLE_SRC" ||
			stored.PhysicalIdentity != fmt.Sprintf("%064x", serial) {
			t.Fatalf("definition changed at the persistence layer: %+v", stored)
		}
	})
}
