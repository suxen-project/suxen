package server

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func retentionUnitPaths(formatName string) spiformat.RetentionUnitPaths {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	units, _ := registered.(spiformat.RetentionUnitPaths)
	return units
}

type retentionUnit struct {
	key    string
	group  string
	newest time.Time
	// version is the unit's ordering version, taken from the asset that
	// established its group when the repository provides one.
	version    string
	hasVersion bool
	assets     []domain.Asset
}

func (unit retentionUnit) rank() retentionRank {
	return newRetentionRank(unit.version, unit.hasVersion, unit.newest)
}

// sortRetentionUnits orders one group's units from most to least retained,
// breaking full ties by key so selection is deterministic.
func sortRetentionUnits(order string, group []retentionUnit) {
	sort.Slice(group, func(i, j int) bool {
		left, right := group[i].rank(), group[j].rank()
		if !rankedNewer(order, left, right) && !rankedNewer(order, right, left) {
			return group[i].key > group[j].key
		}
		return rankedNewer(order, left, right)
	})
}

func retentionUnitDirectory(formatName string) spiformat.RetentionUnitDirectory {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	units, _ := registered.(spiformat.RetentionUnitDirectory)
	return units
}

// selectRetentionDirectoryUnits claims every direct child of a declared
// directory, even when one child does not match. This prevents a partial
// ordinary cleanup from deleting the rest of an incomplete version.
func selectRetentionDirectoryUnits(
	policy domain.CleanupPolicy,
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
	provider spiformat.RetentionUnitDirectory,
	assets []domain.Asset,
	now time.Time,
) ([]retentionUnit, []domain.Asset) {
	if provider == nil {
		return nil, assets
	}
	anchorProvider, requiresAnchor := provider.(spiformat.RetentionUnitAnchor)
	byDirectory := make(map[string][]domain.Asset)
	for _, asset := range assets {
		byDirectory[path.Dir(asset.Path)] = append(byDirectory[path.Dir(asset.Path)], asset)
	}
	claimed := make(map[int64]struct{})
	groups := make(map[string][]retentionUnit)
	seen := make(map[string]struct{})
	for _, asset := range assets {
		directory := provider.RetentionUnitDirectory(repository.FormatView(), asset.Path)
		if directory == "" {
			continue
		}
		if requiresAnchor && !anchorProvider.IsRetentionUnitAnchor(repository.FormatView(), asset.Path) {
			continue
		}
		claimed[asset.ID] = struct{}{}
		if !validCompanionPath(directory) || path.Dir(asset.Path) != directory {
			continue
		}
		if _, done := seen[directory]; done {
			continue
		}
		seen[directory] = struct{}{}
		members := byDirectory[directory]
		unit := retentionUnit{key: directory, assets: members}
		complete := len(members) > 0
		groupSet := false
		for _, member := range members {
			claimed[member.ID] = struct{}{}
			if provider.RetentionUnitDirectory(repository.FormatView(), member.Path) != directory ||
				!cleanupSupportsAsset(member) ||
				!assetMatchesCleanupCriteria(member, repository, policy.Criteria, now) {
				complete = false
			}
			if !requiresAnchor || anchorProvider.IsRetentionUnitAnchor(repository.FormatView(), member.Path) {
				key := cleanupComponent(repository, grouping, member)
				if groupSet && unit.group != key {
					complete = false
				}
				if !groupSet {
					unit.version, unit.hasVersion = retentionVersion(repository, member)
				}
				unit.group = key
				groupSet = true
			}
			if member.UpdatedAt.After(unit.newest) {
				unit.newest = member.UpdatedAt
			}
		}
		if complete && groupSet {
			groups[unit.group] = append(groups[unit.group], unit)
		}
	}
	var selected []retentionUnit
	for _, group := range groups {
		sortRetentionUnits(policy.Order, group)
		keep := policy.KeepLast
		if keep < 0 {
			keep = 0
		}
		selected = append(selected, group[min(keep, len(group)):]...)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].key < selected[j].key })
	ordinary := make([]domain.Asset, 0, len(assets)-len(claimed))
	for _, asset := range assets {
		if _, owned := claimed[asset.ID]; !owned {
			ordinary = append(ordinary, asset)
		}
	}
	return selected, ordinary
}

// selectRetentionUnits removes every declared version file from ordinary row
// cleanup. An incomplete or partially matching version is left alone: only a
// complete unit may compete for keepLast or be deleted as a whole.
func selectRetentionUnits(
	policy domain.CleanupPolicy,
	repository domain.Repository,
	grouping spiformat.RetentionGrouping,
	provider spiformat.RetentionUnitPaths,
	assets []domain.Asset,
	now time.Time,
) ([][]domain.Asset, []domain.Asset) {
	if provider == nil {
		return nil, assets
	}
	byPath := make(map[string]domain.Asset, len(assets))
	for _, asset := range assets {
		byPath[asset.Path] = asset
	}
	claimed := make(map[int64]struct{})
	seen := make(map[string]struct{})
	groups := make(map[string][]retentionUnit)
	for _, asset := range assets {
		paths := provider.RetentionUnitPaths(repository.FormatView(), asset.Path)
		if len(paths) == 0 {
			continue
		}
		claimed[asset.ID] = struct{}{}
		// A faulty declaration must never expand a deletion beyond the format's
		// own version. Reject duplicates, traversal, oversized sets, and sets
		// that do not name the calling path.
		if len(paths) > maxCompanionPaths || !validUnitPaths(paths, asset.Path) {
			continue
		}
		paths = append([]string(nil), paths...)
		sort.Strings(paths)
		for _, memberPath := range paths {
			if member, found := byPath[memberPath]; found {
				claimed[member.ID] = struct{}{}
			}
		}
		key := strings.Join(paths, "\x00")
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		unit := retentionUnit{key: key, group: cleanupComponent(repository, grouping, asset)}
		unit.version, unit.hasVersion = retentionVersion(repository, asset)
		complete := true
		for _, memberPath := range paths {
			member, found := byPath[memberPath]
			if !found || !cleanupSupportsAsset(member) ||
				!assetMatchesCleanupCriteria(member, repository, policy.Criteria, now) ||
				!sameUnitPaths(provider.RetentionUnitPaths(repository.FormatView(), memberPath), paths) ||
				cleanupComponent(repository, grouping, member) != unit.group {
				complete = false
				break
			}
			claimed[member.ID] = struct{}{}
			unit.assets = append(unit.assets, member)
			if member.UpdatedAt.After(unit.newest) {
				unit.newest = member.UpdatedAt
			}
		}
		if complete {
			groups[unit.group] = append(groups[unit.group], unit)
		}
	}
	var selected [][]domain.Asset
	for _, group := range groups {
		sortRetentionUnits(policy.Order, group)
		keep := policy.KeepLast
		if keep < 0 {
			keep = 0
		}
		for _, unit := range group[min(keep, len(group)):] {
			selected = append(selected, unit.assets)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i][0].Path < selected[j][0].Path })
	ordinary := make([]domain.Asset, 0, len(assets)-len(claimed))
	for _, asset := range assets {
		if _, owned := claimed[asset.ID]; !owned {
			ordinary = append(ordinary, asset)
		}
	}
	return selected, ordinary
}

func validUnitPaths(paths []string, own string) bool {
	seen := make(map[string]struct{}, len(paths))
	hasOwn := false
	for _, member := range paths {
		if !validCompanionPath(member) || path.Clean(member) != member {
			return false
		}
		if _, duplicate := seen[member]; duplicate {
			return false
		}
		seen[member] = struct{}{}
		hasOwn = hasOwn || member == own
	}
	return hasOwn
}

func sameUnitPaths(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	sort.Strings(left)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
