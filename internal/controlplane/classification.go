package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// ClassificationStore is the backend capability the classification service
// consumes: the atomic per-repository and instance-default classification
// mutations (rules plus asset relabel) that fold the ownership record into one
// transaction serialized on the classification key. The SQL store satisfies it.
type ClassificationStore interface {
	SaveClassification(context.Context, store.ClassificationSave) error
	DeleteClassification(context.Context, string, store.Ownership) error
	SaveClassificationDefaults(context.Context, store.ClassificationDefaultsSave) error
	DeleteClassificationDefaults(context.Context, store.Ownership) error
}

// SaveClassificationCommand creates or replaces a repository's classification
// rules and relabels its assets.
type SaveClassificationCommand struct {
	Config domain.ClassificationConfig
	Intent Intent
}

// SaveClassificationDefaultsCommand creates or replaces the instance-wide
// classification default and relabels inheriting repositories.
type SaveClassificationDefaultsCommand struct {
	Config domain.ClassificationConfig
	Intent Intent
}

// ClassificationService applies classification mutations and their ownership
// effects atomically. It is stateless beyond its backend.
type ClassificationService struct {
	backend ClassificationStore
}

// NewClassificationService composes the classification command service over its backend.
func NewClassificationService(backend ClassificationStore) *ClassificationService {
	return &ClassificationService{backend: backend}
}

// SaveClassification applies a repository classification mutation (rules + asset
// relabel) and its ownership effect atomically.
func (s *ClassificationService) SaveClassification(ctx context.Context, cmd SaveClassificationCommand) error {
	return s.backend.SaveClassification(ctx, store.ClassificationSave{
		Config:    cmd.Config,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteClassification removes a repository's classification and its ownership record atomically.
func (s *ClassificationService) DeleteClassification(ctx context.Context, repository string, intent Intent) error {
	return s.backend.DeleteClassification(ctx, repository, intent.ownership())
}

// SaveClassificationDefaults applies an instance-wide classification-default
// mutation and its ownership effect atomically.
func (s *ClassificationService) SaveClassificationDefaults(ctx context.Context, cmd SaveClassificationDefaultsCommand) error {
	return s.backend.SaveClassificationDefaults(ctx, store.ClassificationDefaultsSave{
		Config:    cmd.Config,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteClassificationDefaults removes the instance-wide classification default and its ownership record atomically.
func (s *ClassificationService) DeleteClassificationDefaults(ctx context.Context, intent Intent) error {
	return s.backend.DeleteClassificationDefaults(ctx, intent.ownership())
}
