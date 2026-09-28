package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestHostedAllowOverwritePublicationAndUpdate(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	disabled := false
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "locked", Format: "raw", Type: "hosted", AllowOverwrite: &disabled}); err != nil {
		t.Fatal(err)
	}
	asset := domain.Asset{Repository: "locked", Path: "release.tgz", Digest: "sha256:first"}
	if _, err := metadata.PutAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, asset); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	asset.Digest = "sha256:second"
	if _, err := metadata.PutAsset(ctx, asset); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("replacement = %v, want conflict", err)
	}
	stored, err := metadata.Repository(ctx, "locked")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AllowOverwrite == nil || *stored.AllowOverwrite {
		t.Fatalf("stored policy = %v", stored.AllowOverwrite)
	}
	// An omitted update preserves the existing policy.
	stored.AllowOverwrite = nil
	if err := metadata.UpdateRepository(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, asset); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("replacement after omitted update = %v", err)
	}
	enabled := true
	stored.AllowOverwrite = &enabled
	if err := metadata.UpdateRepository(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, asset); err != nil {
		t.Fatalf("replacement after opt-in: %v", err)
	}
	final, err := metadata.Asset(ctx, "locked", asset.Path)
	if err != nil || final.Digest != asset.Digest {
		t.Fatalf("final asset = %+v, %v", final, err)
	}
}

func TestHostedNoOverwriteConcurrentBatchIsAtomic(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	disabled := false
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "locked", Format: "raw", Type: "hosted", AllowOverwrite: &disabled}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, suffix := range []string{"one", "two"} {
		workers.Add(1)
		go func(suffix string) {
			defer workers.Done()
			<-start
			_, err := metadata.PutAssets(ctx, []domain.Asset{
				{Repository: "locked", Path: "package.tgz", Digest: "sha256:" + suffix},
				{Repository: "locked", Path: "package.json", Kind: "metadata", Digest: "sha256:" + suffix},
			})
			results <- err
		}(suffix)
	}
	close(start)
	workers.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrConflict):
			conflict++
		default:
			t.Fatalf("publication error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	packageAsset, err := metadata.Asset(ctx, "locked", "package.tgz")
	if err != nil {
		t.Fatal(err)
	}
	companion, err := metadata.Asset(ctx, "locked", "package.json")
	if err != nil {
		t.Fatal(err)
	}
	if packageAsset.Digest != companion.Digest {
		t.Fatalf("artifact %s and metadata %s diverged", packageAsset.Digest, companion.Digest)
	}
}

func TestHostedOverwriteDefaultsAndUnsupportedTypes(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	for _, test := range []struct {
		format string
		want   bool
	}{
		{"raw", true}, {"maven", true}, {"go", true}, {"oci", true},
		{"npm", false}, {"cargo", false}, {"pypi", false},
	} {
		if got := domain.DefaultAllowOverwrite(test.format); got != test.want {
			t.Fatalf("%s default = %t, want %t", test.format, got, test.want)
		}
	}
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "raw-default", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	stored, err := metadata.Repository(ctx, "raw-default")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AllowOverwrite == nil || !*stored.AllowOverwrite {
		t.Fatalf("raw default policy = %v", stored.AllowOverwrite)
	}
	enabled := true
	if err := (domain.Repository{Name: "proxy", Format: "raw", Type: "proxy", Upstream: "https://example.test", AllowOverwrite: &enabled}).Validate(); !errors.Is(err, domain.ErrInvalidOverwritePolicy) {
		t.Fatalf("proxy policy validation = %v", err)
	}
}

func TestPostgresHostedNoOverwriteConcurrentFirstPublication(t *testing.T) {
	metadata := openCompanionTestStore(t, "postgres")
	ctx := context.Background()
	disabled := false
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "locked", Format: "raw", Type: "hosted", AllowOverwrite: &disabled}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, digest := range []string{"sha256:first", "sha256:second"} {
		workers.Add(1)
		go func(digest string) {
			defer workers.Done()
			<-start
			_, err := metadata.PutAsset(ctx, domain.Asset{Repository: "locked", Path: "new.bin", Digest: digest})
			results <- err
		}(digest)
	}
	close(start)
	workers.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrConflict):
			conflict++
		default:
			t.Fatalf("publication error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestHostedImmutableIdentitySurvivesOverwriteOptIn(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()
	enabled := true
	if err := metadata.CreateRepository(ctx, domain.Repository{Name: "claims", Format: "raw", Type: "hosted", AllowOverwrite: &enabled}); err != nil {
		t.Fatal(err)
	}
	claim := domain.Asset{Repository: "claims", Path: "index-claim/widget/1.0.0.json", Digest: "sha256:first", Kind: "metadata", ImmutableIdentity: true}
	if _, err := metadata.PutAsset(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutAsset(ctx, claim); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	claim.Digest = "sha256:second"
	if _, err := metadata.PutAsset(ctx, claim); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity reassignment = %v", err)
	}
}
