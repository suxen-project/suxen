package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// A publication blocked behind a classification write must use the rules that
// committed before it acquired the SQLite write transaction. Watching the two
// checked-out connections proves the publisher has reached the transaction
// boundary before the rule change is allowed to commit.
func TestSQLitePublicationUsesCommittedClassification(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		name := "repository"
		if defaults {
			name = "inherited-default"
		}
		t.Run(name, func(t *testing.T) {
			metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer metadata.Close()
			ctx := context.Background()
			migrateTestMetadata(t, metadata)
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
				t.Fatal(err)
			}
			oldRules := []domain.ClassificationRule{{Key: "stage", Value: "old"}}
			if defaults {
				if err := metadata.SaveClassificationDefaults(ctx, ClassificationDefaultsSave{Config: domain.ClassificationConfig{Rules: oldRules}}); err != nil {
					t.Fatal(err)
				}
			} else if err := metadata.SaveClassification(ctx, ClassificationSave{Config: domain.ClassificationConfig{
				Repository: "raw", InheritGlobal: true, Rules: oldRules,
			}}); err != nil {
				t.Fatal(err)
			}

			transaction, err := metadata.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer transaction.Rollback()
			if err := lockClassificationRelabel(ctx, transaction); err != nil {
				t.Fatal(err)
			}
			newRules, err := json.Marshal([]domain.ClassificationRule{{Key: "stage", Value: "new"}})
			if err != nil {
				t.Fatal(err)
			}
			if defaults {
				_, err = transaction.ExecContext(ctx, `UPDATE classification_defaults SET rules = ? WHERE singleton = 1`, string(newRules))
			} else {
				_, err = transaction.ExecContext(ctx, `UPDATE classification_rules SET rules = ? WHERE repository_id = (SELECT id FROM repositories WHERE name = 'raw')`, string(newRules))
			}
			if err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)
			go func() {
				_, err := metadata.PutAsset(ctx, domain.Asset{
					Repository: "raw", Path: "new.bin",
					Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
				})
				result <- err
			}()
			deadline := time.After(2 * time.Second)
			for metadata.db.database.Stats().InUse < 2 {
				select {
				case err := <-result:
					t.Fatalf("publication finished before rule transaction committed: %v", err)
				case <-deadline:
					t.Fatal("publication did not reach classification transaction boundary")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			if err := transaction.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			asset, err := metadata.Asset(ctx, "raw", "new.bin")
			if err != nil {
				t.Fatal(err)
			}
			if got := classificationLabel(asset.Attributes, "stage"); got != "new" {
				t.Fatalf("publication retained stale classification.stage=%q", got)
			}
		})
	}
}

func TestSQLiteSameDigestPublicationPreservesAttributesAndRelabeledClassification(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	asset := domain.Asset{
		Repository: "raw", Path: "artifact.bin",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
	}
	stored, err := metadata.PutAsset(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.SetAttributes(ctx, "raw", stored.ID, "scan", map[string]any{"status": "passed"}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.SaveClassification(ctx, ClassificationSave{Config: domain.ClassificationConfig{
		Repository: "raw", InheritGlobal: true,
		Rules: []domain.ClassificationRule{{Key: "stage", Value: "new"}},
	}}); err != nil {
		t.Fatal(err)
	}
	asset.Attributes = map[string]any{"publisher": "replacement"}
	stored, err = metadata.PutAsset(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if got := classificationLabel(stored.Attributes, "stage"); got != "new" {
		t.Fatalf("same-digest publication classification.stage=%q, want new", got)
	}
	if stored.Attributes["scan"] == nil {
		t.Fatalf("same-digest publication removed existing scanner attributes: %+v", stored.Attributes)
	}
	if _, present := stored.Attributes["publisher"]; present {
		t.Fatalf("same-digest publication replaced existing writer attributes: %+v", stored.Attributes)
	}
}
