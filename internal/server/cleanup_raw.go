package server

import (
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	"github.com/suxen-project/suxen/internal/retention"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// repositoryRetentionGrouping returns the retention grouping for one
// repository: the Raw component rules when it declares any, otherwise the
// registered format's capability.
func repositoryRetentionGrouping(repository domain.Repository) spiformat.RetentionGrouping {
	return retention.Grouping(repository)
}

// repositoryRetentionUnitDirectory mirrors repositoryRetentionGrouping for
// directory units.
func repositoryRetentionUnitDirectory(repository domain.Repository) spiformat.RetentionUnitDirectory {
	if components, ok := rawRetention(repository); ok {
		return components
	}
	return retentionUnitDirectory(repository.Format)
}

func rawRetention(repository domain.Repository) (retention.RawComponents, bool) {
	return retention.RawComponentsFor(repository)
}

// retentionVersion is the version an asset contributes to version ordering: a
// Raw component version or an OCI tag. Other assets have none.
func retentionVersion(repository domain.Repository, asset domain.Asset) (string, bool) {
	switch {
	case repository.Format == "raw":
		match, ok := rawcomponent.ForConfig(repository.FormatConfig).Match(asset.Path)
		return match.Version, ok
	case asset.Kind == "oci-manifest" && cleanupSupportsAsset(asset):
		return asset.Reference, true
	default:
		return "", false
	}
}
