package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// DeleteAssetWithCompanions must delete an artifact together with its metadata
// companions atomically, cascade to a companion created after the artifact was
// observed (absent from the caller's snapshot), never remove a non-metadata
// asset sharing a declared path, and delete nothing when the artifact changed.
func TestDeleteAssetWithCompanionsProtectsAndCascadesOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		repository := fmt.Sprintf("companion-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: repository, Format: "raw", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		digest := func(seed string) string { return "sha256:" + strings.Repeat(seed, 64) }

		t.Run("cascade to observed companion", func(t *testing.T) {
			artifact := putCompanionAsset(t, metadata, repository, "a/1/artifact", "raw", digest("a"))
			putCompanionAsset(t, metadata, repository, "a/1/meta.json", "metadata", digest("b"))
			deleted, err := metadata.DeleteAssetWithCompanions(ctx, artifact, []string{"a/1/meta.json"})
			if err != nil || !deleted {
				t.Fatalf("delete = %v, %v", deleted, err)
			}
			assertAbsent(t, metadata, repository, "a/1/artifact")
			assertAbsent(t, metadata, repository, "a/1/meta.json")
		})

		t.Run("cascade to companion created after observation", func(t *testing.T) {
			artifact := putCompanionAsset(t, metadata, repository, "b/1/artifact", "raw", digest("a"))
			// Companion appears only now, after the artifact was observed.
			putCompanionAsset(t, metadata, repository, "b/1/meta.json", "metadata", digest("b"))
			deleted, err := metadata.DeleteAssetWithCompanions(ctx, artifact, []string{"b/1/meta.json"})
			if err != nil || !deleted {
				t.Fatalf("delete = %v, %v", deleted, err)
			}
			assertAbsent(t, metadata, repository, "b/1/meta.json")
		})

		t.Run("non-metadata asset at declared path is protected", func(t *testing.T) {
			artifact := putCompanionAsset(t, metadata, repository, "c/1/artifact", "raw", digest("a"))
			putCompanionAsset(t, metadata, repository, "c/1/other", "raw", digest("b"))
			deleted, err := metadata.DeleteAssetWithCompanions(ctx, artifact, []string{"c/1/other"})
			if err != nil || !deleted {
				t.Fatalf("delete = %v, %v", deleted, err)
			}
			assertAbsent(t, metadata, repository, "c/1/artifact")
			if _, err := metadata.Asset(ctx, repository, "c/1/other"); err != nil {
				t.Fatalf("raw asset at declared companion path was removed: %v", err)
			}
		})

		t.Run("changed artifact deletes nothing", func(t *testing.T) {
			artifact := putCompanionAsset(t, metadata, repository, "d/1/artifact", "raw", digest("a"))
			putCompanionAsset(t, metadata, repository, "d/1/meta.json", "metadata", digest("b"))
			if err := metadata.TouchAsset(ctx, artifact.ID, time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			deleted, err := metadata.DeleteAssetWithCompanions(ctx, artifact, []string{"d/1/meta.json"})
			if err != nil || deleted {
				t.Fatalf("changed artifact delete = %v, %v", deleted, err)
			}
			if _, err := metadata.Asset(ctx, repository, "d/1/artifact"); err != nil {
				t.Fatalf("artifact removed despite change: %v", err)
			}
			if _, err := metadata.Asset(ctx, repository, "d/1/meta.json"); err != nil {
				t.Fatalf("companion orphaned when artifact was kept: %v", err)
			}
		})
	})
}

func openCompanionTestStore(t *testing.T, backend string) *SQLStore {
	t.Helper()
	ctx := context.Background()
	if backend == "sqlite" {
		return openMigratedSQLite(t)
	}
	dsn := os.Getenv("SUXEN_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("SUXEN_TEST_POSTGRES is not configured")
	}
	// The PostgreSQL integration tests share a server. Give each test its own
	// schema so fixed resource names and rows left by an earlier test cannot
	// affect another test or a repeated run.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "suxen_test_" + hex.EncodeToString(suffix[:])
	admin, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop PostgreSQL test schema %s: %v", schema, err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close PostgreSQL test admin connection: %v", err)
		}
	})
	testDSN, err := postgresSchemaDSN(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	postgres, err := OpenPostgres(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = postgres.Close() })
	metadata := postgres.SQLStore
	if err := metadata.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.BlobStore(ctx, "default"); errors.Is(err, domain.ErrNotFound) {
		if err := createTestDefaultBlobStore(ctx, metadata); err != nil && !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func postgresSchemaDSN(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	return dsn + " search_path=" + schema, nil
}

func putCompanionAsset(t *testing.T, metadata *SQLStore, repository, path, kind, digest string) domain.Asset {
	t.Helper()
	asset, err := metadata.PutAsset(context.Background(), domain.Asset{
		Repository: repository, Path: path, Kind: kind, Digest: digest, Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func assertAbsent(t *testing.T, metadata *SQLStore, repository, path string) {
	t.Helper()
	if _, err := metadata.Asset(context.Background(), repository, path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("asset %q still present: %v", path, err)
	}
}
