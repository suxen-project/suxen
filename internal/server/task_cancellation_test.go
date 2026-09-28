package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

type cancelAfterTaskCreationStore struct {
	store.Store
	cancel    context.CancelFunc
	taskID    int64
	migration bool
	updateErr error
}

func (s *cancelAfterTaskCreationStore) UpdateTask(ctx context.Context, task domain.Task) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.UpdateTask(ctx, task)
}

func (s *cancelAfterTaskCreationStore) CreateTask(ctx context.Context, task domain.Task) (domain.Task, error) {
	created, err := s.Store.CreateTask(ctx, task)
	if err == nil {
		s.taskID = created.ID
		s.cancel()
	}
	return created, err
}

func (s *cancelAfterTaskCreationStore) BlobStores(ctx context.Context) ([]domain.BlobStore, error) {
	if s.migration && ctx.Err() == nil {
		return []domain.BlobStore{{Name: "default", State: domain.BlobStoreStateDraining, DrainTarget: "archive"}}, nil
	}
	return s.Store.BlobStores(ctx)
}

func TestCanceledMaintenanceTasksPersistFailure(t *testing.T) {
	for _, name := range []string{"cleanup", "gc", "verify", "migrate"} {
		t.Run(name, func(t *testing.T) {
			fixture := newServerFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapper := &cancelAfterTaskCreationStore{Store: fixture.Metadata, cancel: cancel, migration: name == "migrate"}
			fixture.Handler.metadata = wrapper
			var err error
			switch name {
			case "cleanup":
				_, err = fixture.Handler.runCleanupTask(ctx, domain.CleanupPolicy{Name: "expired", Criteria: domain.CleanupCriteria{{Path: "sys.size", Op: ">", Value: 0}}}, "raw", true)
			case "gc":
				_, err = fixture.Handler.runGarbageCollection(ctx, true, defaultGCGracePeriod, "")
			case "verify":
				_, err = fixture.Handler.runBlobStoreVerify(ctx, "", false)
			case "migrate":
				err = fixture.Handler.runBlobStoreMigration(ctx)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want cancellation", err)
			}
			task, err := fixture.Metadata.Task(context.Background(), wrapper.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != "failed" || task.CompletedAt == nil || !strings.Contains(task.Error, "context canceled") {
				t.Fatalf("cancelled task left unfinished: %+v", task)
			}
		})
	}
}

func TestCanceledTaskRetainsCauseWhenCompletionWriteFails(t *testing.T) {
	fixture := newServerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updateErr := errors.New("task store unavailable")
	fixture.Handler.metadata = &cancelAfterTaskCreationStore{
		Store: fixture.Metadata, cancel: cancel, updateErr: updateErr,
	}
	_, err := fixture.Handler.runBlobStoreVerify(ctx, "", false)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, updateErr) {
		t.Fatalf("error = %v, want both cancellation and completion failure", err)
	}
}
