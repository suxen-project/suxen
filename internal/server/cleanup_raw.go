package server

import (
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	"github.com/suxen-project/suxen/internal/retention"
	"github.com/suxen-project/suxen/internal/store"
)

// rawVersion is one Raw component version: every unit that assigns its
// retention group the same version, once one of them holds an anchor file. A
// rule's unit holds the assets it assigns the same component, version, and
// version directory; a directory unit also owns everything stored below its
// directory. The version is kept or deleted as a whole, so one keepLast slot
// covers all of its units.
type rawVersion struct {
	group    string
	name     string
	version  string
	implicit bool
	units    []rawcomponent.Unit
	rank     retentionRank
	assets   []domain.Asset
}

// sets are what the store must find unchanged to delete the version: every
// pattern-matched row of the component version, wherever a rule placed it,
// and the whole directory of each directory unit.
func (version rawVersion) sets(rules rawcomponent.Rules) []store.AssetSet {
	sets := []store.AssetSet{{
		Component: version.name,
		Version:   version.version,
		Member:    func(assetPath string) bool { return !rules.Match(assetPath).Implicit },
	}}
	for _, unit := range version.units {
		if unit.Directory != "" {
			sets = append(sets, store.AssetSet{Prefix: unit.Directory + "/"})
		}
	}
	return sets
}

// selectRawCleanup returns the Raw versions of the accepted groups that a
// policy deletes. A version competes for keepLast only when one of its units
// holds a file matching its rule's anchor and every unit is complete: every
// file is supported and matches the policy, and, for a directory unit,
// nothing else is stored below its directory. Units of a version without any
// anchor file are left alone. assets must hold every member of the accepted
// groups' versions and everything below their version directories.
func selectRawCleanup(
	policy domain.CleanupPolicy,
	repository domain.Repository,
	assets []domain.Asset,
	acceptGroup func(group string) bool,
	now time.Time,
) []rawVersion {
	type unitState struct {
		group              string
		implicit           bool
		anchored, complete bool
		assets             []domain.Asset
	}
	rules := rawcomponent.ForConfig(repository.FormatConfig)
	units := make(map[rawcomponent.Unit]*unitState)
	directories := make(map[string][]rawcomponent.Unit)
	assetUnits := make([]rawcomponent.Unit, len(assets))
	for index, asset := range assets {
		match := rules.Match(asset.Path)
		unit := match.Unit()
		assetUnits[index] = unit
		state, found := units[unit]
		if !found {
			state = &unitState{group: retention.RawGroupKey(match), implicit: match.Implicit, complete: true}
			units[unit] = state
			if unit.Directory != "" {
				directories[unit.Directory] = append(directories[unit.Directory], unit)
			}
		}
		state.assets = append(state.assets, asset)
		state.anchored = state.anchored || match.Anchor
		if !retention.Supports(asset) || !assetMatchesCleanupCriteria(asset, repository, policy.Criteria, now) {
			state.complete = false
		}
	}
	// A file below a version directory that belongs to another unit keeps
	// that directory from being deleted as a whole.
	for index, asset := range assets {
		for parent := asset.Path; ; {
			slash := strings.LastIndex(parent, "/")
			if slash < 0 {
				break
			}
			parent = parent[:slash]
			for _, unit := range directories[parent] {
				if unit != assetUnits[index] {
					units[unit].complete = false
				}
			}
		}
	}
	type versionKey struct{ group, version string }
	versions := make(map[versionKey]*rawVersion)
	anchored := make(map[versionKey]bool)
	blocked := make(map[versionKey]bool)
	for unit, state := range units {
		if !acceptGroup(state.group) {
			continue
		}
		key := versionKey{state.group, unit.Version}
		anchored[key] = anchored[key] || state.anchored
		blocked[key] = blocked[key] || !state.complete
		version := versions[key]
		if version == nil {
			version = &rawVersion{group: state.group, name: unit.Name, version: unit.Version, implicit: state.implicit}
			versions[key] = version
		}
		version.units = append(version.units, unit)
		version.assets = append(version.assets, state.assets...)
	}
	groups := make(map[string][]*rawVersion)
	for key, version := range versions {
		if !anchored[key] || blocked[key] {
			continue
		}
		newest := version.assets[0].UpdatedAt
		for _, asset := range version.assets {
			if asset.UpdatedAt.After(newest) {
				newest = asset.UpdatedAt
			}
		}
		sort.Slice(version.assets, func(i, j int) bool { return version.assets[i].Path < version.assets[j].Path })
		sort.Slice(version.units, func(i, j int) bool {
			return version.units[i].Rule < version.units[j].Rule ||
				version.units[i].Rule == version.units[j].Rule && version.units[i].Directory < version.units[j].Directory
		})
		version.rank = newRetentionRank(version.version, true, newest)
		groups[version.group] = append(groups[version.group], version)
	}
	var selected []rawVersion
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			left, right := group[i], group[j]
			if rankedNewer(policy.Order, left.rank, right.rank) != rankedNewer(policy.Order, right.rank, left.rank) {
				return rankedNewer(policy.Order, left.rank, right.rank)
			}
			return left.assets[0].Path > right.assets[0].Path
		})
		for _, version := range group[min(max(policy.KeepLast, 0), len(group)):] {
			selected = append(selected, *version)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].assets[0].Path < selected[j].assets[0].Path })
	return selected
}
