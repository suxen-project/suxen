package server

import (
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// rawComponentGroupPrefix keeps component group keys apart from the parent
// directory keys of unmatched paths; Raw paths never contain a NUL byte.
const rawComponentGroupPrefix = "\x00raw-component\x00"

// rawComponentRetention adapts a Raw repository's component rules to the
// retention hooks plugin formats implement, so the host's unit selection and
// atomic deletion apply unchanged. Unmatched paths report no grouping or unit
// and keep the default directory grouping and per-file cleanup.
type rawComponentRetention struct {
	rules rawcomponent.Rules
}

func (retention rawComponentRetention) RetentionGroupKey(
	_ spiformat.Repository,
	asset spiformat.Asset,
) (string, bool) {
	match, ok := retention.rules.Match(asset.Path)
	if !ok {
		return "", false
	}
	return rawComponentGroupPrefix + match.Name, true
}

// CompanionPaths declares none: side files are directory unit members instead.
func (rawComponentRetention) CompanionPaths(spiformat.Repository, string) []string {
	return nil
}

func (retention rawComponentRetention) RetentionUnitDirectory(_ spiformat.Repository, assetPath string) string {
	match, ok := retention.rules.Match(assetPath)
	if !ok {
		return ""
	}
	return match.Directory
}

func (retention rawComponentRetention) IsRetentionUnitAnchor(_ spiformat.Repository, assetPath string) bool {
	match, ok := retention.rules.Match(assetPath)
	return ok && match.Anchor
}

// repositoryRetentionGrouping returns the retention grouping for one
// repository: the Raw component rules when it declares any, otherwise the
// registered format's capability.
func repositoryRetentionGrouping(repository domain.Repository) spiformat.RetentionGrouping {
	if retention, ok := rawRetention(repository); ok {
		return retention
	}
	return retentionGrouping(repository.Format)
}

// repositoryRetentionUnitDirectory mirrors repositoryRetentionGrouping for
// directory units.
func repositoryRetentionUnitDirectory(repository domain.Repository) spiformat.RetentionUnitDirectory {
	if retention, ok := rawRetention(repository); ok {
		return retention
	}
	return retentionUnitDirectory(repository.Format)
}

func rawRetention(repository domain.Repository) (rawComponentRetention, bool) {
	if repository.Format != "raw" {
		return rawComponentRetention{}, false
	}
	rules := rawcomponent.ForConfig(repository.FormatConfig)
	return rawComponentRetention{rules: rules}, len(rules) > 0
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
