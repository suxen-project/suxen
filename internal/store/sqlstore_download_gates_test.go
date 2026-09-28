package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestSQLiteDownloadGateInheritGlobalRoundTrips(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := metadata.CreateRepository(ctx, domain.Repository{
		Name:   "raw",
		Format: "raw",
		Type:   "hosted",
	}); err != nil {
		t.Fatal(err)
	}

	// A new repository gate defaults to inheriting the instance-wide default.
	if err := metadata.SetDownloadGate(ctx, domain.DownloadGate{
		Repository:    "raw",
		Criteria:      []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}},
		Enabled:       true,
		InheritGlobal: true,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := metadata.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.InheritGlobal {
		t.Fatalf("stored gate should inherit the global default: %+v", stored)
	}

	// Opting out persists as an empty-criteria row carrying inheritGlobal=false.
	if err := metadata.SetDownloadGate(ctx, domain.DownloadGate{
		Repository:    "raw",
		Criteria:      nil,
		Enabled:       true,
		InheritGlobal: false,
	}); err != nil {
		t.Fatalf("empty-criteria opt-out row should be valid: %v", err)
	}
	optedOut, err := metadata.DownloadGate(ctx, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if optedOut.InheritGlobal {
		t.Fatalf("opt-out gate still inherits the global default: %+v", optedOut)
	}
	if len(optedOut.Criteria) != 0 {
		t.Fatalf("opt-out gate carries unexpected criteria: %+v", optedOut.Criteria)
	}
}

func TestSQLiteDownloadGateDefaultsLifecycle(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)

	if _, err := metadata.DownloadGateDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unset default should report not found, got %v", err)
	}

	if err := metadata.SetDownloadGateDefaults(ctx, domain.DownloadGate{
		Criteria: []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}},
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := metadata.DownloadGateDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Criteria) != 1 || stored.Criteria[0].Value != "passed" || !stored.Enabled {
		t.Fatalf("unexpected stored default: %+v", stored)
	}
	if stored.Repository != "" {
		t.Fatalf("instance-wide default should carry no repository: %+v", stored)
	}

	// A second write replaces the singleton rather than inserting a new row.
	if err := metadata.SetDownloadGateDefaults(ctx, domain.DownloadGate{
		Criteria: nil,
		Enabled:  false,
	}); err != nil {
		t.Fatal(err)
	}
	replaced, err := metadata.DownloadGateDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Criteria) != 0 || replaced.Enabled {
		t.Fatalf("default was not replaced: %+v", replaced)
	}

	if err := metadata.DeleteDownloadGateDefaults(ctx, Ownership{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.DownloadGateDefaults(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted default should report not found, got %v", err)
	}
	if err := metadata.DeleteDownloadGateDefaults(ctx, Ownership{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleting an absent default should report not found, got %v", err)
	}
}

func TestSQLiteGroupRejectsDownloadGateWrites(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "member", Format: "raw", Type: "hosted"},
		{Name: "group", Format: "raw", Type: "group", Members: []string{"member"}},
	} {
		if err := metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	gate := domain.DownloadGate{
		Repository: "group",
		Enabled:    true,
		Criteria:   []domain.Predicate{{Path: "scan.status", Op: "=", Value: "passed"}},
	}
	for _, write := range []struct {
		name string
		run  func() error
	}{
		{"SetDownloadGate", func() error { return metadata.SetDownloadGate(ctx, gate) }},
		{"SaveDownloadGate", func() error {
			return metadata.SaveDownloadGate(ctx, DownloadGateSave{Gate: gate})
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			if err := write.run(); !errors.Is(err, domain.ErrInvalidDownloadGate) {
				t.Fatalf("group gate error = %v, want ErrInvalidDownloadGate", err)
			}
			if _, err := metadata.DownloadGate(ctx, "group"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("group gate was stored: %v", err)
			}
		})
	}
}
