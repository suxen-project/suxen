package server

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/startup"
	"github.com/suxen-project/suxen/internal/store"
)

type failingMigrationStore struct {
	store.Store
	err error
}

func (s failingMigrationStore) Migrate(context.Context) error { return s.err }

func TestBootstrapClassifiesMigrationFailure(t *testing.T) {
	for _, trial := range []struct {
		name      string
		err       error
		retryable bool
	}{
		{"unavailable database", errors.New("database unavailable"), true},
		{"incompatible schema", &store.SchemaError{Err: errors.New("schema is newer")}, false},
	} {
		t.Run(trial.name, func(t *testing.T) {
			fixture := newServerFixture(t)
			fixture.Handler.metadata = failingMigrationStore{Store: fixture.Metadata, err: trial.err}
			_, err := fixture.Handler.Bootstrap(context.Background())
			if !errors.Is(err, trial.err) || startup.ShouldRetry(err) != trial.retryable {
				t.Fatalf("Bootstrap() error = %v, retryable = %v", err, startup.ShouldRetry(err))
			}
		})
	}
}

func TestExtraListenerBindConflictStopsStartup(t *testing.T) {
	fixture := newServerFixture(t)
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	port := blocker.Addr().(*net.TCPAddr).Port
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "blocked-oci", Format: "oci", Type: "hosted",
		Endpoints: &domain.RepositoryEndpoints{Ports: []int{port}},
	}); err != nil {
		t.Fatal(err)
	}
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.Listen = "127.0.0.1:8080" })
	err = fixture.Handler.StartExtraListeners(context.Background())
	if err == nil || startup.ShouldRetry(err) {
		t.Fatalf("StartExtraListeners() error = %v, want permanent bind conflict", err)
	}
}
