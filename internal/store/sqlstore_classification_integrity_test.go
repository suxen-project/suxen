package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestClassificationUsesStoredAssetProjection(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := metadata.CreateRepository(ctx, domain.Repository{
				Name: "raw", Format: "raw", Type: "proxy", Upstream: "https://example.com",
			}); err != nil {
				t.Fatal(err)
			}
			asset, err := metadata.PutAsset(ctx, domain.Asset{
				Repository: "raw", Path: "internal-cache-key", FormatPath: "visible/file",
				Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			checks := []struct {
				key, path, op, value string
			}{
				{"path", "sys.path", "=", "visible/file"},
				{"access", "sys.lastAccessed", "after", "2000-01-01T00:00:00Z"},
				{"validation", "sys.validatedAt", "after", "2000-01-01T00:00:00Z"},
			}
			rules := make([]domain.ClassificationRule, 0, len(checks))
			for _, check := range checks {
				rules = append(rules, domain.ClassificationRule{
					When: []domain.Predicate{{Path: check.path, Op: check.op, Value: check.value}},
					Key:  check.key, Value: "yes",
				})
			}
			if _, err := metadata.SetClassification(ctx, domain.ClassificationConfig{
				Repository: "raw", Rules: rules,
			}); err != nil {
				t.Fatal(err)
			}
			asset, err = metadata.Asset(ctx, "raw", asset.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range checks {
				if got := classificationLabel(asset.Attributes, check.key); got != "yes" {
					t.Errorf("%s label = %q, want yes", check.path, got)
				}
			}
		})
	}
}

func TestClassificationPublicationUsesPersistedFields(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.SetClassification(ctx, domain.ClassificationConfig{
				Repository: "raw", Rules: []domain.ClassificationRule{
					{When: []domain.Predicate{{Path: "sys.validatedAt", Op: "after", Value: "2000-01-01T00:00:00Z"}}, Key: "validated", Value: "yes"},
					{When: []domain.Predicate{{Path: "sys.lastAccessed", Op: "after", Value: "2000-01-01T00:00:00Z"}}, Key: "accessed", Value: "yes"},
					{When: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}}, Key: "approved", Value: "yes"},
				},
			}); err != nil {
				t.Fatal(err)
			}
			input := domain.Asset{Repository: "raw", Path: "file", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
				Attributes: map[string]any{"scan": map[string]any{"status": "passed"}}}
			asset, err := metadata.PutAsset(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"validated", "accessed", "approved"} {
				if got := classificationLabel(asset.Attributes, key); got != "yes" {
					t.Errorf("initial classification.%s = %q", key, got)
				}
			}
			if err := metadata.SetAttributes(ctx, "raw", asset.ID, "scan", map[string]any{"status": "rejected"}); err != nil {
				t.Fatal(err)
			}
			asset, err = metadata.PutAsset(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if got := classificationLabel(asset.Attributes, "approved"); got != "" {
				t.Errorf("same-digest publication classified stale incoming scan status: %q", got)
			}
			if got := asset.Attributes["scan"].(map[string]any)["status"]; got != "rejected" {
				t.Errorf("same-digest publication replaced persisted scan status: %v", got)
			}
		})
	}
}

func TestClassificationDoesNotUsePreviousLabelsAsRuleInput(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			rules := []domain.ClassificationRule{{
				When: []domain.Predicate{{Path: "classification.stage", Op: "absent"}},
				Key:  "stage", Value: "release",
			}}
			config := domain.ClassificationConfig{Repository: "raw", Rules: rules}
			if _, err := metadata.SetClassification(ctx, config); err != nil {
				t.Fatal(err)
			}
			input := domain.Asset{Repository: "raw", Path: "file", Digest: "sha256:" + strings.Repeat("b", 64), Size: 1}
			asset, err := metadata.PutAsset(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if got := classificationLabel(asset.Attributes, "stage"); got != "release" {
				t.Fatalf("initial label = %q", got)
			}
			if _, err := metadata.SetClassification(ctx, config); err != nil {
				t.Fatal(err)
			}
			asset, err = metadata.Asset(ctx, "raw", "file")
			if err != nil {
				t.Fatal(err)
			}
			if got := classificationLabel(asset.Attributes, "stage"); got != "release" {
				t.Errorf("label after identical rule update = %q", got)
			}
			asset, err = metadata.PutAsset(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if got := classificationLabel(asset.Attributes, "stage"); got != "release" {
				t.Errorf("label after same-digest publication = %q", got)
			}
		})
	}
}

func TestPostgresRelabelReadsCurrentAnnotationAndValidation(t *testing.T) {
	for _, change := range []string{"replace", "delete", "validate"} {
		for _, scope := range []string{"repository", "defaults", "repository-direct", "defaults-direct"} {
			t.Run(scope+"/"+change, func(t *testing.T) {
				metadata := openCompanionTestStore(t, "postgres")
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
					t.Fatal(err)
				}
				asset, err := metadata.PutAsset(ctx, domain.Asset{
					Repository: "raw", Path: "file", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
					Attributes: map[string]any{"scan": map[string]any{"status": "passed"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				writer, err := metadata.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback()
				var writerPID int
				if err := writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
					t.Fatal(err)
				}
				var predicate domain.Predicate
				want := ""
				switch change {
				case "replace":
					predicate = domain.Predicate{Path: "scan.status", Op: "=", Value: "rejected"}
					want = "yes"
					_, err = writer.ExecContext(ctx, `UPDATE assets SET attributes = ? WHERE id = ?`, `{"scan":{"status":"rejected"}}`, asset.ID)
				case "delete":
					predicate = domain.Predicate{Path: "scan.status", Op: "=", Value: "passed"}
					_, err = writer.ExecContext(ctx, `UPDATE assets SET attributes = ? WHERE id = ?`, `{}`, asset.ID)
				case "validate":
					predicate = domain.Predicate{Path: "sys.validatedAt", Op: "after", Value: "2100-01-01T00:00:00Z"}
					want = "yes"
					_, err = writer.ExecContext(ctx, `UPDATE assets SET validated_at = ? WHERE id = ?`, "2101-01-01T00:00:00Z", asset.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				rules := []domain.ClassificationRule{{When: []domain.Predicate{predicate}, Key: "matched", Value: "yes"}}
				result := make(chan error, 1)
				go func() {
					switch scope {
					case "defaults":
						result <- metadata.SaveClassificationDefaults(ctx, ClassificationDefaultsSave{Config: domain.ClassificationConfig{Rules: rules}})
					case "repository":
						result <- metadata.SaveClassification(ctx, ClassificationSave{Config: domain.ClassificationConfig{Repository: "raw", Rules: rules}})
					case "defaults-direct":
						_, err := metadata.SetClassificationDefaults(ctx, domain.ClassificationConfig{Rules: rules})
						result <- err
					case "repository-direct":
						_, err := metadata.SetClassification(ctx, domain.ClassificationConfig{Repository: "raw", Rules: rules})
						result <- err
					}
				}()
				for {
					var blocked bool
					if err := metadata.db.QueryRowContext(ctx, `SELECT EXISTS (
						SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid))
						AND query LIKE '%assets%')`, writerPID).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case err := <-result:
						t.Fatalf("relabel ended before writer committed: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
				if err := writer.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
				asset, err = metadata.Asset(ctx, "raw", "file")
				if err != nil {
					t.Fatal(err)
				}
				if got := classificationLabel(asset.Attributes, "matched"); got != want {
					t.Fatalf("classification.matched = %q, want %q", got, want)
				}
				if change == "replace" && asset.Attributes["scan"].(map[string]any)["status"] != "rejected" {
					t.Fatalf("relabel restored stale scanner annotation: %+v", asset.Attributes)
				}
				if change == "delete" {
					if _, present := asset.Attributes["scan"]; present {
						t.Fatalf("relabel restored deleted scanner annotation: %+v", asset.Attributes)
					}
				}
			})
		}
	}
}
