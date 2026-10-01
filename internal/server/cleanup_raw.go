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
	// fileUnits maps each path of a file-named version to every stored path
	// sharing its rule, component, and version. Only versions with an anchor
	// file are present.
	fileUnits map[string][]string
}

// withFileUnits indexes the file-named versions among assets so
// RetentionUnitPaths can answer from a path alone.
func (retention rawComponentRetention) withFileUnits(assets []domain.Asset) rawComponentRetention {
	type unitKey struct {
		rule          int
		name, version string
	}
	members := make(map[unitKey][]string)
	anchored := make(map[unitKey]bool)
	for _, asset := range assets {
		match, ok := retention.rules.Match(asset.Path)
		if !ok || match.Directory != "" {
			continue
		}
		key := unitKey{rule: match.Rule, name: match.Name, version: match.Version}
		members[key] = append(members[key], asset.Path)
		anchored[key] = anchored[key] || match.Anchor
	}
	retention.fileUnits = make(map[string][]string)
	for key, paths := range members {
		if !anchored[key] {
			continue
		}
		for _, memberPath := range paths {
			retention.fileUnits[memberPath] = paths
		}
	}
	return retention
}

// RetentionUnitPaths declares a file-named version: every asset sharing the
// rule, component, and version, provided one of them is an anchor.
func (retention rawComponentRetention) RetentionUnitPaths(_ spiformat.Repository, assetPath string) []string {
	return retention.fileUnits[assetPath]
}

// unitMember reports whether a stored path belongs to the same version unit
// as assetPath, for the deletion-time completeness check.
func (retention rawComponentRetention) unitMember(assetPath string) (prefix string, member func(string) bool, ok bool) {
	match, ok := retention.rules.Match(assetPath)
	if !ok {
		return "", nil, false
	}
	return retention.rules.ScanPrefix(match), func(candidate string) bool {
		other, matched := retention.rules.Match(candidate)
		return matched && rawcomponent.SameUnit(match, other)
	}, true
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
