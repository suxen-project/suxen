// Package retention computes the keepLast grouping that cleanup uses, so the
// store can persist each asset's group at write time and cleanup can read one
// group at a time instead of a whole repository.
package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// rawComponentGroupPrefix keeps component group keys apart from the parent
// directory keys of unmatched paths; Raw paths never contain control bytes,
// and the prefix stays storable in PostgreSQL text, which rejects NUL.
const rawComponentGroupPrefix = "\x01raw-component\x01"

// maxStoredGroupLength keeps a stored group key well inside index entry limits.
const maxStoredGroupLength = 1024

// Supports reports whether cleanup may select the asset at all: Raw-kind files
// and tagged OCI manifests.
func Supports(asset domain.Asset) bool {
	if asset.Kind == "raw" {
		return true
	}
	return asset.Kind == "oci-manifest" &&
		asset.Reference != "" &&
		!strings.HasPrefix(asset.Reference, "sha256:")
}

// FormatGrouping returns the registered format's optional retention-grouping
// capability, or nil when the format does not own its grouping.
func FormatGrouping(formatName string) spiformat.RetentionGrouping {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	grouping, _ := registered.(spiformat.RetentionGrouping)
	return grouping
}

// Grouping returns the retention grouping for one repository: the Raw
// component rules when it declares any, otherwise the format's capability.
func Grouping(repository domain.Repository) spiformat.RetentionGrouping {
	if components, ok := RawComponentsFor(repository); ok {
		return components
	}
	return FormatGrouping(repository.Format)
}

// GroupKey is the keepLast grouping key: the identity whose versions compete
// for retention. A format that owns its grouping decides the key; otherwise
// OCI manifests group by image name and everything else by parent directory.
func GroupKey(repository domain.Repository, grouping spiformat.RetentionGrouping, asset domain.Asset) string {
	if grouping != nil {
		if key, ok := grouping.RetentionGroupKey(repository.FormatView(), asset.FormatView()); ok {
			return key
		}
	}
	if asset.Kind == "oci-manifest" {
		if route, ok := ocimodel.ParseAssetPath(asset.Path); ok && route.Kind == ocimodel.ManifestRoute {
			return route.ImageName
		}
	}
	directory := path.Dir(asset.Path)
	if directory == "." {
		return ""
	}
	return directory
}

// StoredGroup is the value persisted in assets.retention_group: empty for
// assets cleanup never selects, otherwise StoredKey of the group key.
func StoredGroup(repository domain.Repository, asset domain.Asset) string {
	if !Supports(asset) {
		return ""
	}
	return StoredKey(GroupKey(repository, Grouping(repository), asset))
}

// StoredKey encodes a group key for storage. Keys are stored verbatim behind a
// "g" marker, so the root directory's empty key stays distinct from "no
// group"; a key that is too long, invalid UTF-8, or holds NUL is replaced by
// its digest behind an "h" marker, which preserves equality.
func StoredKey(key string) string {
	if len(key) < maxStoredGroupLength && utf8.ValidString(key) && !strings.ContainsRune(key, 0) {
		return "g" + key
	}
	digest := sha256.Sum256([]byte(key))
	return "h" + hex.EncodeToString(digest[:])
}

// RawComponents adapts a Raw repository's component rules to the retention
// hooks plugin formats implement, so the host's unit selection and atomic
// deletion apply unchanged. Unmatched paths report no grouping or unit and
// keep the default directory grouping and per-file cleanup.
type RawComponents struct {
	Rules rawcomponent.Rules
	// fileUnits maps each path of a file-named version to every stored path
	// sharing its rule, component, and version. Only versions with an anchor
	// file are present.
	fileUnits map[string][]string
}

// RawComponentsFor returns the adapter for a Raw repository with patterns.
func RawComponentsFor(repository domain.Repository) (RawComponents, bool) {
	if repository.Format != "raw" {
		return RawComponents{}, false
	}
	rules := rawcomponent.ForConfig(repository.FormatConfig)
	return RawComponents{Rules: rules}, len(rules) > 0
}

// WithFileUnits indexes the file-named versions among assets so
// RetentionUnitPaths can answer from a path alone. The assets must include
// every member of each version they touch.
func (components RawComponents) WithFileUnits(assets []domain.Asset) RawComponents {
	type unitKey struct {
		rule          int
		name, version string
	}
	members := make(map[unitKey][]string)
	anchored := make(map[unitKey]bool)
	for _, asset := range assets {
		match, ok := components.Rules.Match(asset.Path)
		if !ok || match.Directory != "" {
			continue
		}
		key := unitKey{rule: match.Rule, name: match.Name, version: match.Version}
		members[key] = append(members[key], asset.Path)
		anchored[key] = anchored[key] || match.Anchor
	}
	components.fileUnits = make(map[string][]string)
	for key, paths := range members {
		if !anchored[key] {
			continue
		}
		for _, memberPath := range paths {
			components.fileUnits[memberPath] = paths
		}
	}
	return components
}

// RetentionUnitPaths declares a file-named version: every asset sharing the
// rule, component, and version, provided one of them is an anchor.
func (components RawComponents) RetentionUnitPaths(_ spiformat.Repository, assetPath string) []string {
	return components.fileUnits[assetPath]
}

// UnitMember reports whether a stored path belongs to the same version unit
// as assetPath, for the deletion-time completeness check.
func (components RawComponents) UnitMember(assetPath string) (prefix string, member func(string) bool, ok bool) {
	match, ok := components.Rules.Match(assetPath)
	if !ok {
		return "", nil, false
	}
	return components.Rules.ScanPrefix(match), func(candidate string) bool {
		other, matched := components.Rules.Match(candidate)
		return matched && rawcomponent.SameUnit(match, other)
	}, true
}

func (components RawComponents) RetentionGroupKey(_ spiformat.Repository, asset spiformat.Asset) (string, bool) {
	match, ok := components.Rules.Match(asset.Path)
	if !ok {
		return "", false
	}
	return rawComponentGroupPrefix + match.Name, true
}

// CompanionPaths declares none: side files are unit members instead.
func (RawComponents) CompanionPaths(spiformat.Repository, string) []string {
	return nil
}

func (components RawComponents) RetentionUnitDirectory(_ spiformat.Repository, assetPath string) string {
	match, ok := components.Rules.Match(assetPath)
	if !ok {
		return ""
	}
	return match.Directory
}

func (components RawComponents) IsRetentionUnitAnchor(_ spiformat.Repository, assetPath string) bool {
	match, ok := components.Rules.Match(assetPath)
	return ok && match.Anchor
}
