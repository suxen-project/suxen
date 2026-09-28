package content

import (
	"context"
	"errors"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/predicate"
)

func (rt *Runtime) DownloadAllowed(
	ctx context.Context,
	asset domain.Asset,
) (string, error) {
	provenanceAllowed, err := rt.provenanceDownloadAllowed(ctx, asset)
	if err != nil {
		return "", err
	}
	if !provenanceAllowed {
		return "provenance_required", nil
	}
	return rt.downloadGateAllowed(ctx, asset)
}

// downloadGateAllowed evaluates the download gate independently of provenance.
// The OCI signing bootstrap may skip provenance, but must still pass this gate.
func (rt *Runtime) downloadGateAllowed(
	ctx context.Context,
	asset domain.Asset,
) (string, error) {
	gate, gated, err := rt.effectiveDownloadGate(ctx, asset)
	if err != nil {
		return "", err
	}
	if !gated || !gate.Enabled {
		return "", nil
	}
	var repository domain.Repository
	if asset.RepositoryID != "" {
		repository, err = rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID}).Repository(ctx)
	} else {
		repository, err = rt.repositoryResolver().Repository(ctx, asset.Repository)
	}
	if err != nil {
		return "", err
	}
	attributes := assetattrs.Project(asset, repository)
	if !predicate.MatchAll(attributes, gate.Criteria, time.Now().UTC()) {
		return "download_gated", nil
	}
	return "", nil
}

// effectiveDownloadGate resolves a repository's read gate against the
// instance-wide default. When the repository inherits (the default for a repo
// with no gate row, or one whose InheritGlobal is set) the default's criteria
// AND-extend the repository's; a repository row that opts out uses its own
// criteria only. Enabled follows the repository's row when one exists, else the
// default's. The bool is false when neither layer defines a gate.
func (rt *Runtime) effectiveDownloadGate(
	ctx context.Context,
	asset domain.Asset,
) (domain.DownloadGate, bool, error) {
	var repoGate domain.DownloadGate
	var err error
	if asset.RepositoryID != "" {
		repoGate, err = rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID}).DownloadGate(ctx)
	} else {
		repoGate, err = rt.policyReader().DownloadGate(ctx, asset.Repository)
	}
	hasRepo := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.DownloadGate{}, false, err
	}
	inherit := true
	if hasRepo {
		inherit = repoGate.InheritGlobal
	}
	var global domain.DownloadGate
	hasGlobal := false
	if inherit {
		global, err = rt.policyReader().DownloadGateDefaults(ctx)
		if err == nil {
			hasGlobal = true
		} else if !errors.Is(err, domain.ErrNotFound) {
			return domain.DownloadGate{}, false, err
		}
	}
	if !hasRepo && !hasGlobal {
		return domain.DownloadGate{}, false, nil
	}
	effective := domain.DownloadGate{Repository: asset.Repository}
	if inherit && hasGlobal {
		effective.Criteria = append(
			append([]domain.Predicate{}, global.Criteria...),
			repoGate.Criteria...,
		)
		if hasRepo {
			effective.Enabled = repoGate.Enabled
		} else {
			effective.Enabled = global.Enabled
		}
	} else {
		effective.Criteria = repoGate.Criteria
		effective.Enabled = repoGate.Enabled
	}
	return effective, true, nil
}
