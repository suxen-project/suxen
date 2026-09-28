package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func classificationLabel(attributes map[string]any, key string) string {
	classification, _ := attributes["classification"].(map[string]any)
	value, _ := classification[key].(string)
	return value
}

func TestSQLiteClassificationInstanceDefaults(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	// Three repositories: alpha and gamma inherit the default; beta opts out.
	digit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: name, Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: name, Path: "artifact.bin", Digest: "sha256:" + digit, Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// beta opts out with its own rule; the later global default must not touch it.
	if _, err := metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository:    "beta",
		Rules:         []domain.ClassificationRule{{Key: "tier", Value: "internal"}},
		InheritGlobal: false,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := metadata.ClassificationDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unset default should report not found, got %v", err)
	}

	// Setting the default retroactively relabels every inheriting repository.
	relabeled, err := metadata.SetClassificationDefaults(ctx, domain.ClassificationConfig{
		Rules: []domain.ClassificationRule{{Key: "tier", Value: "public"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if relabeled != 2 {
		t.Fatalf("relabeled %d assets, want 2 (alpha, gamma)", relabeled)
	}
	assertLabel(t, metadata, "alpha", "tier", "public")
	assertLabel(t, metadata, "gamma", "tier", "public")
	assertLabel(t, metadata, "beta", "tier", "internal") // opted out, untouched

	// A repository rule with the same key overrides the inherited default.
	if _, err := metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository:    "alpha",
		Rules:         []domain.ClassificationRule{{Key: "tier", Value: "team"}},
		InheritGlobal: true,
	}); err != nil {
		t.Fatal(err)
	}
	assertLabel(t, metadata, "alpha", "tier", "team")

	// A newly ingested asset in an inheriting repository picks up the default.
	if _, err := metadata.PutAsset(ctx, domain.Asset{
		Repository: "gamma", Path: "second.bin",
		Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := metadata.Asset(ctx, "gamma", "second.bin")
	if err != nil {
		t.Fatal(err)
	}
	if label := classificationLabel(second.Attributes, "tier"); label != "public" {
		t.Fatalf("newly ingested asset tier = %q, want public", label)
	}

	// Deleting the default reverts inheriting repositories to their own rules:
	// gamma (no own rules) loses the label; alpha keeps its own.
	if err := metadata.DeleteClassificationDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
	assertLabel(t, metadata, "gamma", "tier", "")
	assertLabel(t, metadata, "alpha", "tier", "team")
	assertLabel(t, metadata, "beta", "tier", "internal")

	if _, err := metadata.ClassificationDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted default should report not found, got %v", err)
	}
}

// TestSQLiteClassificationOwnershipRelabelResolvesTargetsInTransaction exercises
// the ownership-aware relabel path (SaveClassification / SaveClassificationDefaults),
// which resolves its inheriting-repository target set inside the mutation
// transaction under the shared relabel lock rather than from a pre-transaction
// read. A per-repository opt-out committed before the default write must exclude
// that repository from the default's relabel.
func TestSQLiteClassificationOwnershipRelabelResolvesTargetsInTransaction(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	digit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, name := range []string{"alpha", "beta"} {
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: name, Format: "raw", Type: "hosted",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.PutAsset(ctx, domain.Asset{
			Repository: name, Path: "artifact.bin", Digest: "sha256:" + digit, Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// beta opts out through the ownership-aware command before the default write.
	if err := metadata.SaveClassification(ctx, ClassificationSave{
		Config: domain.ClassificationConfig{
			Repository:    "beta",
			Rules:         []domain.ClassificationRule{{Key: "tier", Value: "internal"}},
			InheritGlobal: false,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The default write must resolve its targets from committed state and leave
	// beta's opt-out labels intact.
	if err := metadata.SaveClassificationDefaults(ctx, ClassificationDefaultsSave{
		Config: domain.ClassificationConfig{
			Rules: []domain.ClassificationRule{{Key: "tier", Value: "public"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertLabel(t, metadata, "alpha", "tier", "public")
	assertLabel(t, metadata, "beta", "tier", "internal")
}

func assertLabel(t *testing.T, metadata *SQLStore, repository, key, want string) {
	t.Helper()
	asset, err := metadata.Asset(context.Background(), repository, "artifact.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := classificationLabel(asset.Attributes, key); got != want {
		t.Fatalf("%s classification.%s = %q, want %q", repository, key, got, want)
	}
}
