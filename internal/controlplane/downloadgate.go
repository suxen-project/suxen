package controlplane

import (
	"context"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// DownloadGateStore is the backend capability the download-gate service
// consumes: the atomic per-repository and instance-default read-gate mutations
// that fold the ownership record into one transaction serialized on the gate
// key. The SQL store satisfies it.
type DownloadGateStore interface {
	SaveDownloadGate(context.Context, store.DownloadGateSave) error
	DeleteDownloadGate(context.Context, string, store.Ownership) error
	SaveDownloadGateDefaults(context.Context, store.DownloadGateDefaultsSave) error
	DeleteDownloadGateDefaults(context.Context, store.Ownership) error
}

// SaveDownloadGateCommand creates or replaces a repository's attribute-based
// read gate.
type SaveDownloadGateCommand struct {
	Gate   domain.DownloadGate
	Intent Intent
}

// SaveDownloadGateDefaultsCommand creates or replaces the instance-wide
// download-gate default.
type SaveDownloadGateDefaultsCommand struct {
	Gate   domain.DownloadGate
	Intent Intent
}

// DownloadGateService applies download-gate mutations and their ownership
// effects atomically. It is stateless beyond its backend.
type DownloadGateService struct {
	backend DownloadGateStore
}

// NewDownloadGateService composes the download-gate command service over its backend.
func NewDownloadGateService(backend DownloadGateStore) *DownloadGateService {
	return &DownloadGateService{backend: backend}
}

// SaveDownloadGate applies a repository read-gate mutation and its ownership
// effect atomically.
func (s *DownloadGateService) SaveDownloadGate(ctx context.Context, cmd SaveDownloadGateCommand) error {
	return s.backend.SaveDownloadGate(ctx, store.DownloadGateSave{
		Gate:      cmd.Gate,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteDownloadGate removes a repository's read gate and its ownership record atomically.
func (s *DownloadGateService) DeleteDownloadGate(ctx context.Context, repository string, intent Intent) error {
	return s.backend.DeleteDownloadGate(ctx, repository, intent.ownership())
}

// SaveDownloadGateDefaults applies an instance-wide download-gate-default
// mutation and its ownership effect atomically.
func (s *DownloadGateService) SaveDownloadGateDefaults(ctx context.Context, cmd SaveDownloadGateDefaultsCommand) error {
	return s.backend.SaveDownloadGateDefaults(ctx, store.DownloadGateDefaultsSave{
		Gate:      cmd.Gate,
		Ownership: cmd.Intent.ownership(),
	})
}

// DeleteDownloadGateDefaults removes the instance-wide download-gate default and its ownership record atomically.
func (s *DownloadGateService) DeleteDownloadGateDefaults(ctx context.Context, intent Intent) error {
	return s.backend.DeleteDownloadGateDefaults(ctx, intent.ownership())
}
