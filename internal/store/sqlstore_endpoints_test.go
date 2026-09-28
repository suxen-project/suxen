package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestSQLiteRepositoryEndpointsRoundTripAndConflict(t *testing.T) {
	metadata := openMigratedSQLite(t)
	ctx := context.Background()

	docker := domain.Repository{
		Name:   "docker",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Hosts: []string{"Registry.Example.com"},
			Ports: []int{5000},
		},
	}
	if err := metadata.CreateRepository(ctx, docker); err != nil {
		t.Fatal(err)
	}
	stored, err := metadata.Repository(ctx, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Endpoints == nil || stored.Endpoints.Hosts[0] != "registry.example.com" {
		t.Fatalf("stored endpoints = %+v", stored.Endpoints)
	}

	conflict := domain.Repository{
		Name:   "images",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Hosts: []string{"registry.example.com"},
		},
	}
	if err := metadata.CreateRepository(ctx, conflict); !errors.Is(err, domain.ErrOCIEndpointConflict) {
		t.Fatalf("overlapping host CreateRepository() = %v, want ErrOCIEndpointConflict", err)
	}

	images := domain.Repository{
		Name:   "images",
		Format: "oci",
		Type:   "hosted",
		Endpoints: &domain.RepositoryEndpoints{
			Ports: []int{5001},
		},
	}
	if err := metadata.CreateRepository(ctx, images); err != nil {
		t.Fatal(err)
	}
	images.Endpoints = &domain.RepositoryEndpoints{Ports: []int{5000}}
	if err := metadata.UpdateRepository(ctx, images); !errors.Is(err, domain.ErrOCIEndpointConflict) {
		t.Fatalf("overlapping port UpdateRepository() = %v, want ErrOCIEndpointConflict", err)
	}
}
