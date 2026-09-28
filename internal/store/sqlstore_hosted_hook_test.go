package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

type configuredMutableFormat struct{}

func (configuredMutableFormat) Name() string                                             { return "storehostedhook" }
func (configuredMutableFormat) ValidateRepository(repository spiformat.Repository) error { return nil }
func (configuredMutableFormat) MutableHostedPath(repository spiformat.Repository, assetPath string) bool {
	suffix, _ := repository.Config["mutableSuffix"].(string)
	return !repository.AllowOverwrite && suffix != "" && strings.HasSuffix(assetPath, suffix)
}

func TestMutableHostedPathReceivesCurrentStoredFormatConfig(t *testing.T) {
	spiformat.Register(configuredMutableFormat{})
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	disabled := false
	repository := domain.Repository{
		Name: "configured", Format: "storehostedhook", Type: "hosted", AllowOverwrite: &disabled,
		FormatConfig: map[string]any{"mutableSuffix": ".index"},
	}
	if err := metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	put := func(path, digest string) error {
		_, err := metadata.PutAsset(ctx, domain.Asset{Repository: repository.Name, Path: path, Digest: digest})
		return err
	}
	if err := put("catalog.index", "sha256:first"); err != nil {
		t.Fatal(err)
	}
	if err := put("catalog.index", "sha256:second"); err != nil {
		t.Fatalf("configured mutable path: %v", err)
	}
	if err := put("artifact.bin", "sha256:first"); err != nil {
		t.Fatal(err)
	}
	if err := put("artifact.bin", "sha256:second"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("artifact replacement = %v", err)
	}
	stored, err := metadata.Repository(ctx, repository.Name)
	if err != nil {
		t.Fatal(err)
	}
	stored.FormatConfig = map[string]any{"mutableSuffix": ".catalog"}
	if err := metadata.UpdateRepository(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := put("catalog.index", "sha256:third"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("obsolete mutable path = %v", err)
	}
	if err := put("new.catalog", "sha256:first"); err != nil {
		t.Fatal(err)
	}
	if err := put("new.catalog", "sha256:second"); err != nil {
		t.Fatalf("updated mutable path: %v", err)
	}
}
